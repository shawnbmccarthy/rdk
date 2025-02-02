package builtin

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	goutils "go.viam.com/utils"

	"go.viam.com/rdk/components/base"
	"go.viam.com/rdk/components/base/kinematicbase"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/services/motion/builtin/state"
	"go.viam.com/rdk/services/slam"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/spatialmath"
)

const (
	defaultReplanCostFactor = 1.0
	defaultMaxReplans       = -1 // Values below zero will replan infinitely
	baseStopTimeout         = time.Second * 5
)

// validatedMotionConfiguration is a copy of the motion.MotionConfiguration type
// which has been validated to conform to the expectations of the builtin
// motion servicl.
type validatedMotionConfiguration struct {
	obstacleDetectors     []motion.ObstacleDetectorName
	positionPollingFreqHz float64
	obstaclePollingFreqHz float64
	planDeviationMM       float64
	linearMPerSec         float64
	angularDegsPerSec     float64
}

type requestType uint8

const (
	requestTypeUnspecified requestType = iota
	requestTypeMoveOnGlobe
	requestTypeMoveOnMap
)

// moveRequest is a structure that contains all the information necessary for to make a move call.
type moveRequest struct {
	requestType requestType
	// origin is only set if requestType == requestTypeMoveOnGlobe
	origin            spatialmath.GeoPose
	logger            logging.Logger
	config            *validatedMotionConfiguration
	planRequest       *motionplan.PlanRequest
	seedPlan          motionplan.Plan
	kinematicBase     kinematicbase.KinematicBase
	obstacleDetectors map[vision.Service][]resource.Name
	replanCostFactor  float64

	ctx               context.Context
	cancelFn          context.CancelFunc
	backgroundWorkers *sync.WaitGroup
	responseChan      chan moveResponse
	// replanners for the move request
	// if we ever have to add additional instances we should figure out how to make this more scalable
	position, obstacle *replanner
	// waypointIndex tracks the waypoint we are currently executing on
	waypointIndex *atomic.Int32
}

// plan creates a plan using the currentInputs of the robot and the moveRequest's planRequest.
func (mr *moveRequest) Plan() (state.PlanResponse, error) {
	inputs, err := mr.kinematicBase.CurrentInputs(mr.ctx)
	if err != nil {
		return state.PlanResponse{}, err
	}
	// TODO: this is really hacky and we should figure out a better place to store this information
	if len(mr.kinematicBase.Kinematics().DoF()) == 2 {
		inputs = inputs[:2]
	}
	mr.planRequest.StartConfiguration = map[string][]referenceframe.Input{mr.kinematicBase.Kinematics().Name(): inputs}

	// TODO(RSDK-5634): this should pass in mr.seedplan and the appropriate replanCostFactor once this bug is found and fixed.
	plan, err := motionplan.Replan(mr.ctx, mr.planRequest, nil, 0)
	if err != nil {
		return state.PlanResponse{}, err
	}

	waypoints, err := plan.GetFrameSteps(mr.kinematicBase.Kinematics().Name())
	if err != nil {
		return state.PlanResponse{}, err
	}

	switch mr.requestType {
	case requestTypeMoveOnMap:
		return state.PlanResponse{
			Waypoints:  waypoints,
			Motionplan: plan,
		}, nil
	case requestTypeMoveOnGlobe:
		return state.PlanResponse{
			Waypoints:  waypoints,
			Motionplan: plan,
		}, nil
	case requestTypeUnspecified:
		fallthrough
	default:
		return state.PlanResponse{}, fmt.Errorf("invalid moveRequest.requestType: %d", mr.requestType)
	}
}

// execute attempts to follow a given Plan starting from the index percribed by waypointIndex.
// Note that waypointIndex is an atomic int that is incremented in this function after each waypoint has been successfully reached.
func (mr *moveRequest) execute(ctx context.Context, waypoints state.Waypoints, waypointIndex *atomic.Int32) (state.ExecuteResponse, error) {
	// Iterate through the list of waypoints and issue a command to move to each
	for i := int(waypointIndex.Load()); i < len(waypoints); i++ {
		select {
		case <-ctx.Done():
			mr.logger.Debugf("calling kinematicBase.Stop due to %s\n", ctx.Err())
			if stopErr := mr.stop(); stopErr != nil {
				return state.ExecuteResponse{}, errors.Wrap(ctx.Err(), stopErr.Error())
			}
			return state.ExecuteResponse{}, nil
		default:
			mr.planRequest.Logger.Info(waypoints[i])
			if err := mr.kinematicBase.GoToInputs(ctx, waypoints[i]); err != nil {
				// If there is an error on GoToInputs, stop the component if possible before returning the error
				mr.logger.Debugf("calling kinematicBase.Stop due to %s\n", err)
				if stopErr := mr.stop(); stopErr != nil {
					return state.ExecuteResponse{}, errors.Wrap(err, stopErr.Error())
				}
				return state.ExecuteResponse{}, err
			}
			if i < len(waypoints)-1 {
				waypointIndex.Add(1)
			}
		}
	}
	// the plan has been fully executed so check to see if where we are at is close enough to the goal.
	return mr.deviatedFromPlan(ctx, waypoints, len(waypoints)-1)
}

// deviatedFromPlan takes a list of waypoints and an index of a waypoint on that Plan and returns whether or not it is still
// following the plan as described by the PlanDeviation specified for the moveRequest.
func (mr *moveRequest) deviatedFromPlan(ctx context.Context, waypoints state.Waypoints, waypointIndex int) (state.ExecuteResponse, error) {
	errorState, err := mr.kinematicBase.ErrorState(ctx, waypoints, waypointIndex)
	if err != nil {
		return state.ExecuteResponse{}, err
	}
	if errorState.Point().Norm() > mr.config.planDeviationMM {
		msg := "error state exceeds planDeviationMM; planDeviationMM: %f, errorstate.Point().Norm(): %f, errorstate.Point(): %#v "
		reason := fmt.Sprintf(msg, mr.config.planDeviationMM, errorState.Point().Norm(), errorState.Point())
		return state.ExecuteResponse{Replan: true, ReplanReason: reason}, nil
	}
	return state.ExecuteResponse{}, nil
}

func (mr *moveRequest) obstaclesIntersectPlan(
	ctx context.Context,
	waypoints state.Waypoints,
	waypointIndex int,
) (state.ExecuteResponse, error) {
	var plan motionplan.Plan
	// We only care to check against waypoints we have not reached yet.
	for _, inputs := range waypoints[waypointIndex:] {
		input := make(map[string][]referenceframe.Input)
		input[mr.kinematicBase.Name().Name] = inputs
		plan = append(plan, input)
	}

	for visSrvc, cameraNames := range mr.obstacleDetectors {
		for _, camName := range cameraNames {
			// get detections from vision service
			detections, err := visSrvc.GetObjectPointClouds(ctx, camName.Name, nil)
			if err != nil {
				return state.ExecuteResponse{}, err
			}

			// Note: detections are initially observed from the camera frame but must be transformed to be in
			// world frame. We cannot use the inputs of the base to transform the detections since they are relative

			// get the current position of the base which we will use to transform the detection into world coordinates
			currentPosition, err := mr.kinematicBase.CurrentPosition(ctx)
			if err != nil {
				return state.ExecuteResponse{}, err
			}

			// Any obstacles specified by the worldstate of the moveRequest will also re-detected here.
			// There is no need to append the new detections to the existing worldstate.
			// We can safely build from scratch without excluding any valuable information.
			geoms := []spatialmath.Geometry{}
			for i, detection := range detections {
				geometry := detection.Geometry.Transform(currentPosition.Pose())
				label := camName.Name + "_transientObstacle_" + strconv.Itoa(i)
				if geometry.Label() != "" {
					label += "_" + geometry.Label()
				}
				geometry.SetLabel(label)
				geoms = append(geoms, geometry)
			}
			gif := referenceframe.NewGeometriesInFrame(referenceframe.World, geoms)
			tf, err := mr.planRequest.FrameSystem.Transform(mr.planRequest.StartConfiguration, gif, mr.planRequest.FrameSystem.World().Name())
			if err != nil {
				return state.ExecuteResponse{}, err
			}
			transformedGIF, ok := tf.((*referenceframe.GeometriesInFrame))
			if !ok {
				// TODO: This is not an acceptable error
				return state.ExecuteResponse{}, errors.New("smth")
			}
			gifs := []*referenceframe.GeometriesInFrame{transformedGIF}
			worldState, err := referenceframe.NewWorldState(gifs, nil)
			if err != nil {
				return state.ExecuteResponse{}, err
			}

			currentInputs, err := mr.kinematicBase.CurrentInputs(ctx)
			if err != nil {
				return state.ExecuteResponse{}, err
			}

			// get the pose difference between where the robot is versus where it ought to be.
			errorState, err := mr.kinematicBase.ErrorState(ctx, waypoints, waypointIndex)
			if err != nil {
				return state.ExecuteResponse{}, err
			}

			if err := motionplan.CheckPlan(
				mr.kinematicBase.Kinematics(), // frame we wish to check for collisions
				plan,                          // remainder of plan we wish to check against
				worldState,                    // detected obstacles by this instance of camera + service
				mr.planRequest.FrameSystem,
				currentPosition.Pose(), // currentPosition of robot accounts for errorState
				currentInputs,
				errorState, // deviation of robot from plan
				lookAheadDistanceMM,
				mr.planRequest.Logger,
			); err != nil {
				mr.planRequest.Logger.Info(err.Error())
				return state.ExecuteResponse{Replan: true, ReplanReason: err.Error()}, nil
			}
		}
	}
	return state.ExecuteResponse{}, nil
}

func kbOptionsFromCfg(motionCfg *validatedMotionConfiguration, validatedExtra validatedExtra) kinematicbase.Options {
	kinematicsOptions := kinematicbase.NewKinematicBaseOptions()

	if motionCfg.linearMPerSec > 0 {
		kinematicsOptions.LinearVelocityMMPerSec = motionCfg.linearMPerSec * 1000
	}

	if motionCfg.angularDegsPerSec > 0 {
		kinematicsOptions.AngularVelocityDegsPerSec = motionCfg.angularDegsPerSec
	}

	if motionCfg.planDeviationMM > 0 {
		kinematicsOptions.PlanDeviationThresholdMM = motionCfg.planDeviationMM
	}

	if validatedExtra.motionProfile != "" {
		kinematicsOptions.PositionOnlyMode = validatedExtra.motionProfile == motionplan.PositionOnlyMotionProfile
	}

	kinematicsOptions.GoalRadiusMM = motionCfg.planDeviationMM
	kinematicsOptions.HeadingThresholdDegrees = 8
	return kinematicsOptions
}

func validateNotNan(f float64, name string) error {
	if math.IsNaN(f) {
		return errors.Errorf("%s may not be NaN", name)
	}
	return nil
}

func validateNotNeg(f float64, name string) error {
	if f < 0 {
		return errors.Errorf("%s may not be negative", name)
	}
	return nil
}

func validateNotNegNorNaN(f float64, name string) error {
	if err := validateNotNan(f, name); err != nil {
		return err
	}
	return validateNotNeg(f, name)
}

func newValidatedMotionCfg(motionCfg *motion.MotionConfiguration) (*validatedMotionConfiguration, error) {
	vmc := &validatedMotionConfiguration{}
	if motionCfg == nil {
		return vmc, nil
	}

	if err := validateNotNegNorNaN(motionCfg.LinearMPerSec, "LinearMPerSec"); err != nil {
		return vmc, err
	}

	if err := validateNotNegNorNaN(motionCfg.AngularDegsPerSec, "AngularDegsPerSec"); err != nil {
		return vmc, err
	}

	if err := validateNotNegNorNaN(motionCfg.PlanDeviationMM, "PlanDeviationMM"); err != nil {
		return vmc, err
	}

	if err := validateNotNegNorNaN(motionCfg.ObstaclePollingFreqHz, "ObstaclePollingFreqHz"); err != nil {
		return vmc, err
	}

	if err := validateNotNegNorNaN(motionCfg.PositionPollingFreqHz, "PositionPollingFreqHz"); err != nil {
		return vmc, err
	}

	vmc.linearMPerSec = motionCfg.LinearMPerSec
	vmc.angularDegsPerSec = motionCfg.AngularDegsPerSec
	vmc.planDeviationMM = motionCfg.PlanDeviationMM
	vmc.obstaclePollingFreqHz = motionCfg.ObstaclePollingFreqHz
	vmc.positionPollingFreqHz = motionCfg.PositionPollingFreqHz
	vmc.obstacleDetectors = motionCfg.ObstacleDetectors
	return vmc, nil
}

func (ms *builtIn) newMoveOnGlobeRequest(
	ctx context.Context,
	req motion.MoveOnGlobeReq,
	seedPlan motionplan.Plan,
	replanCount int,
) (*moveRequest, error) {
	valExtra, err := newValidatedExtra(req.Extra)
	if err != nil {
		return nil, err
	}

	if valExtra.maxReplans >= 0 {
		if replanCount > valExtra.maxReplans {
			return nil, fmt.Errorf("exceeded maximum number of replans: %d", valExtra.maxReplans)
		}
	}

	motionCfg, err := newValidatedMotionCfg(req.MotionCfg)
	if err != nil {
		return nil, err
	}
	// ensure arguments are well behaved
	obstacles := req.Obstacles
	if obstacles == nil {
		obstacles = []*spatialmath.GeoObstacle{}
	}
	if req.Destination == nil {
		return nil, errors.New("destination cannot be nil")
	}

	if math.IsNaN(req.Destination.Lat()) || math.IsNaN(req.Destination.Lng()) {
		return nil, errors.New("destination may not contain NaN")
	}

	// build kinematic options
	kinematicsOptions := kbOptionsFromCfg(motionCfg, valExtra)

	// build the localizer from the movement sensor
	movementSensor, ok := ms.movementSensors[req.MovementSensorName]
	if !ok {
		return nil, resource.DependencyNotFoundError(req.MovementSensorName)
	}
	origin, _, err := movementSensor.Position(ctx, nil)
	if err != nil {
		return nil, err
	}

	heading, err := movementSensor.CompassHeading(ctx, nil)
	if err != nil {
		return nil, err
	}

	// add an offset between the movement sensor and the base if it is applicable
	baseOrigin := referenceframe.NewPoseInFrame(req.ComponentName.ShortName(), spatialmath.NewZeroPose())
	movementSensorToBase, err := ms.fsService.TransformPose(ctx, baseOrigin, movementSensor.Name().ShortName(), nil)
	if err != nil {
		// here we make the assumption the movement sensor is coincident with the base
		movementSensorToBase = baseOrigin
	}
	localizer := motion.NewMovementSensorLocalizer(movementSensor, origin, movementSensorToBase.Pose())

	// create a KinematicBase from the componentName
	baseComponent, ok := ms.components[req.ComponentName]
	if !ok {
		return nil, resource.NewNotFoundError(req.ComponentName)
	}
	b, ok := baseComponent.(base.Base)
	if !ok {
		return nil, fmt.Errorf("cannot move component of type %T because it is not a Base", baseComponent)
	}

	fs, err := ms.fsService.FrameSystem(ctx, nil)
	if err != nil {
		return nil, err
	}

	// Important: GeoPointToPose will create a pose such that incrementing latitude towards north increments +Y, and incrementing
	// longitude towards east increments +X. Heading is not taken into account. This pose must therefore be transformed based on the
	// orientation of the base such that it is a pose relative to the base's current location.
	goalPoseRaw := spatialmath.NewPoseFromPoint(spatialmath.GeoPointToPoint(req.Destination, origin))
	// construct limits
	straightlineDistance := goalPoseRaw.Point().Norm()
	if straightlineDistance > maxTravelDistanceMM {
		return nil, fmt.Errorf("cannot move more than %d kilometers", int(maxTravelDistanceMM*1e-6))
	}
	limits := []referenceframe.Limit{
		{Min: -straightlineDistance * 3, Max: straightlineDistance * 3},
		{Min: -straightlineDistance * 3, Max: straightlineDistance * 3},
		{Min: -2 * math.Pi, Max: 2 * math.Pi},
	} // Note: this is only for diff drive, not used for PTGs
	ms.logger.Debugf("base limits: %v", limits)

	kb, err := kinematicbase.WrapWithKinematics(ctx, b, ms.logger, localizer, limits, kinematicsOptions)
	if err != nil {
		return nil, err
	}

	geomsRaw := spatialmath.GeoObstaclesToGeometries(obstacles, origin)

	mr, err := ms.relativeMoveRequestFromAbsolute(
		ctx,
		motionCfg,
		ms.logger,
		kb,
		goalPoseRaw,
		fs,
		geomsRaw,
		valExtra,
	)
	if err != nil {
		return nil, err
	}
	mr.seedPlan = seedPlan
	mr.replanCostFactor = valExtra.replanCostFactor
	mr.requestType = requestTypeMoveOnGlobe
	mr.origin = *spatialmath.NewGeoPose(origin, heading)
	return mr, nil
}

// newMoveOnMapRequest instantiates a moveRequest intended to be used in the context of a MoveOnMap call.
func (ms *builtIn) newMoveOnMapRequest(
	ctx context.Context,
	req motion.MoveOnMapReq,
) (*moveRequest, error) {
	valExtra, err := newValidatedExtra(req.Extra)
	if err != nil {
		return nil, err
	}
	// get the SLAM Service from the slamName
	slamSvc, ok := ms.slamServices[req.SlamName]
	if !ok {
		return nil, resource.DependencyNotFoundError(req.SlamName)
	}

	// gets the extents of the SLAM map
	limits, err := slam.Limits(ctx, slamSvc)
	if err != nil {
		return nil, err
	}
	limits = append(limits, referenceframe.Limit{Min: -2 * math.Pi, Max: 2 * math.Pi})

	// create a KinematicBase from the componentName
	component, ok := ms.components[req.ComponentName]
	if !ok {
		return nil, resource.DependencyNotFoundError(req.ComponentName)
	}
	b, ok := component.(base.Base)
	if !ok {
		return nil, fmt.Errorf("cannot move component of type %T because it is not a Base", component)
	}

	motionCfg, err := newValidatedMotionCfg(nil)
	if err != nil {
		return nil, err
	}

	// build kinematic options
	kinematicsOptions := kbOptionsFromCfg(motionCfg, valExtra)

	fs, err := ms.fsService.FrameSystem(ctx, nil)
	if err != nil {
		return nil, err
	}

	kb, err := kinematicbase.WrapWithKinematics(ctx, b, ms.logger, motion.NewSLAMLocalizer(slamSvc), limits, kinematicsOptions)
	if err != nil {
		return nil, err
	}
	goalPoseAdj := spatialmath.Compose(req.Destination, motion.SLAMOrientationAdjustment)

	// get point cloud data in the form of bytes from pcd
	pointCloudData, err := slam.PointCloudMapFull(ctx, slamSvc)
	if err != nil {
		return nil, err
	}
	// store slam point cloud data  in the form of a recursive octree for collision checking
	octree, err := pointcloud.ReadPCDToBasicOctree(bytes.NewReader(pointCloudData))
	if err != nil {
		return nil, err
	}

	mr, err := ms.relativeMoveRequestFromAbsolute(
		ctx,
		motionCfg,
		ms.logger,
		kb,
		goalPoseAdj,
		fs,
		[]spatialmath.Geometry{octree},
		valExtra,
	)
	mr.requestType = requestTypeMoveOnMap
	return mr, err
}

func (ms *builtIn) relativeMoveRequestFromAbsolute(
	ctx context.Context,
	motionCfg *validatedMotionConfiguration,
	logger logging.Logger,
	kb kinematicbase.KinematicBase,
	goalPoseInWorld spatialmath.Pose,
	fs referenceframe.FrameSystem,
	worldObstacles []spatialmath.Geometry,
	valExtra validatedExtra,
) (*moveRequest, error) {
	// replace original base frame with one that knows how to move itself and allow planning for
	kinematicFrame := kb.Kinematics()
	if err := fs.ReplaceFrame(kinematicFrame); err != nil {
		return nil, err
	}
	// We want to disregard anything in the FS whose eventual parent is not the base, because we don't know where it is.
	baseOnlyFS, err := fs.FrameSystemSubset(kinematicFrame)
	if err != nil {
		return nil, err
	}

	startPose, err := kb.CurrentPosition(ctx)
	if err != nil {
		return nil, err
	}
	startPoseInv := spatialmath.PoseInverse(startPose.Pose())

	goal := referenceframe.NewPoseInFrame(referenceframe.World, spatialmath.PoseBetween(startPose.Pose(), goalPoseInWorld))

	// convert GeoObstacles into GeometriesInFrame with respect to the base's starting point
	geoms := make([]spatialmath.Geometry, 0, len(worldObstacles))
	for _, geom := range worldObstacles {
		geoms = append(geoms, geom.Transform(startPoseInv))
	}

	gif := referenceframe.NewGeometriesInFrame(referenceframe.World, geoms)
	worldState, err := referenceframe.NewWorldState([]*referenceframe.GeometriesInFrame{gif}, nil)
	ms.logger.Infof("startPose: %v", spatialmath.PoseToProtobuf(startPose.Pose()))
	ms.logger.Infof("requested world goal: %v", spatialmath.PoseToProtobuf(goalPoseInWorld))
	if err != nil {
		return nil, err
	}

	obstacleDetectors := make(map[vision.Service][]resource.Name)
	for _, obstacleDetectorNamePair := range motionCfg.obstacleDetectors {
		// get vision service
		visionServiceName := obstacleDetectorNamePair.VisionServiceName
		visionSvc, ok := ms.visionServices[visionServiceName]
		if !ok {
			return nil, resource.DependencyNotFoundError(visionServiceName)
		}

		// add camera to vision service map
		camList, ok := obstacleDetectors[visionSvc]
		if !ok {
			obstacleDetectors[visionSvc] = []resource.Name{obstacleDetectorNamePair.CameraName}
		} else {
			camList = append(camList, obstacleDetectorNamePair.CameraName)
			obstacleDetectors[visionSvc] = camList
		}
	}

	currentInputs, _, err := ms.fsService.CurrentInputs(ctx)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFn := context.WithCancel(context.Background())
	var backgroundWorkers sync.WaitGroup

	var waypointIndex atomic.Int32
	waypointIndex.Store(1)

	// effectively don't poll if the PositionPollingFreqHz is not provided
	positionPollingFreq := time.Duration(math.MaxInt64)
	if motionCfg.positionPollingFreqHz > 0 {
		positionPollingFreq = time.Duration(1000/motionCfg.positionPollingFreqHz) * time.Millisecond
	}

	// effectively don't poll if the ObstaclePollingFreqHz is not provided
	obstaclePollingFreq := time.Duration(math.MaxInt64)
	if motionCfg.obstaclePollingFreqHz > 0 {
		obstaclePollingFreq = time.Duration(1000/motionCfg.obstaclePollingFreqHz) * time.Millisecond
	}

	mr := &moveRequest{
		config: motionCfg,
		logger: ms.logger,
		planRequest: &motionplan.PlanRequest{
			Logger:             logger,
			Goal:               goal,
			Frame:              kinematicFrame,
			FrameSystem:        baseOnlyFS,
			StartConfiguration: currentInputs,
			WorldState:         worldState,
			Options:            valExtra.extra,
		},
		kinematicBase:     kb,
		replanCostFactor:  valExtra.replanCostFactor,
		obstacleDetectors: obstacleDetectors,

		ctx:               cancelCtx,
		cancelFn:          cancelFn,
		backgroundWorkers: &backgroundWorkers,

		responseChan: make(chan moveResponse, 1),

		waypointIndex: &waypointIndex,
	}

	// TODO: Change deviatedFromPlan to just query positionPollingFreq on the struct & the same for the obstaclesIntersectPlan
	mr.position = newReplanner(positionPollingFreq, mr.deviatedFromPlan)
	mr.obstacle = newReplanner(obstaclePollingFreq, mr.obstaclesIntersectPlan)
	return mr, nil
}

type moveResponse struct {
	err             error
	executeResponse state.ExecuteResponse
}

func (mr moveResponse) String() string {
	return fmt.Sprintf("builtin.moveResponse{executeResponse: %#v, err: %v}", mr.executeResponse, mr.err)
}

func (mr *moveRequest) start(waypoints [][]referenceframe.Input) {
	mr.backgroundWorkers.Add(1)
	goutils.ManagedGo(func() {
		mr.position.startPolling(mr.ctx, waypoints, mr.waypointIndex)
	}, mr.backgroundWorkers.Done)

	mr.backgroundWorkers.Add(1)
	goutils.ManagedGo(func() {
		mr.obstacle.startPolling(mr.ctx, waypoints, mr.waypointIndex)
	}, mr.backgroundWorkers.Done)

	// spawn function to execute the plan on the robot
	mr.backgroundWorkers.Add(1)
	goutils.ManagedGo(func() {
		executeResp, err := mr.execute(mr.ctx, waypoints, mr.waypointIndex)
		resp := moveResponse{executeResponse: executeResp, err: err}
		mr.responseChan <- resp
	}, mr.backgroundWorkers.Done)
}

func (mr *moveRequest) listen() (state.ExecuteResponse, error) {
	select {
	case <-mr.ctx.Done():
		mr.logger.Debugf("context err: %s", mr.ctx.Err())
		mr.Cancel()
		return state.ExecuteResponse{}, mr.ctx.Err()

	case resp := <-mr.responseChan:
		mr.logger.Debugf("execution response: %s", resp)
		mr.Cancel()
		return resp.executeResponse, resp.err

	case resp := <-mr.position.responseChan:
		mr.logger.Debugf("position response: %s", resp)
		mr.Cancel()
		return resp.executeResponse, resp.err

	case resp := <-mr.obstacle.responseChan:
		mr.logger.Debugf("obstacle response: %s", resp)
		mr.Cancel()
		return resp.executeResponse, resp.err
	}
}

func (mr *moveRequest) Execute(waypoints state.Waypoints) (state.ExecuteResponse, error) {
	mr.start(waypoints)
	return mr.listen()
}

// cancel cleans up a moveRequest
// it cancels the processes spawned by it, drains all the channels that could have been written to and waits on processes to return.
func (mr *moveRequest) Cancel() {
	mr.cancelFn()
	mr.backgroundWorkers.Wait()
}

func (mr *moveRequest) stop() error {
	stopCtx, cancelFn := context.WithTimeout(context.Background(), baseStopTimeout)
	defer cancelFn()
	if stopErr := mr.kinematicBase.Stop(stopCtx, nil); stopErr != nil {
		mr.logger.Errorf("kinematicBase.Stop returned error %s", stopErr)
		return stopErr
	}
	return nil
}
