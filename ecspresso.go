package ecspresso

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Songmu/prompter"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/applicationautoscaling"
	"github.com/aws/aws-sdk-go/service/cloudwatchevents"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/codedeploy"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/kayac/go-config"
	"github.com/mattn/go-isatty"
	"github.com/morikuni/aec"
	"github.com/pkg/errors"
)

const KeepDesiredCount = -1

var isTerminal = isatty.IsTerminal(os.Stdout.Fd())
var TerminalWidth = 90
var delayForServiceChanged = 3 * time.Second
var spcIndent = "  "

func taskDefinitionName(t *ecs.TaskDefinition) string {
	return fmt.Sprintf("%s:%d", *t.Family, *t.Revision)
}

type App struct {
	ecs         *ecs.ECS
	autoScaling *applicationautoscaling.ApplicationAutoScaling
	codedeploy  *codedeploy.CodeDeploy
	cwl         *cloudwatchlogs.CloudWatchLogs
	cwe         *cloudwatchevents.CloudWatchEvents
	Service     string
	Cluster     string
	config      *Config
	Debug       bool
}

func (d *App) DescribeServicesInput() *ecs.DescribeServicesInput {
	return &ecs.DescribeServicesInput{
		Cluster:  aws.String(d.Cluster),
		Services: []*string{aws.String(d.Service)},
	}
}

func (d *App) DescribeTasksInput(task *ecs.Task) *ecs.DescribeTasksInput {
	return &ecs.DescribeTasksInput{
		Cluster: aws.String(d.Cluster),
		Tasks:   []*string{task.TaskArn},
	}
}

func (d *App) GetLogEventsInput(logGroup string, logStream string, startAt int64) *cloudwatchlogs.GetLogEventsInput {
	return &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(logGroup),
		LogStreamName: aws.String(logStream),
		StartTime:     aws.Int64(startAt),
	}
}

func (d *App) DescribeServiceStatus(ctx context.Context, events int) (*ecs.Service, error) {
	out, err := d.ecs.DescribeServicesWithContext(ctx, d.DescribeServicesInput())
	if err != nil {
		return nil, errors.Wrap(err, "failed to describe service")
	}
	if len(out.Services) == 0 {
		return nil, errors.New("service is not found")
	}
	s := out.Services[0]
	fmt.Println("Service:", *s.ServiceName)
	fmt.Println("Cluster:", arnToName(*s.ClusterArn))
	fmt.Println("TaskDefinition:", arnToName(*s.TaskDefinition))
	if len(s.Deployments) > 0 {
		fmt.Println("Deployments:")
		for _, dep := range s.Deployments {
			fmt.Println(spcIndent + formatDeployment(dep))
		}
	}
	if len(s.TaskSets) > 0 {
		fmt.Println("TaskSets:")
		for _, ts := range s.TaskSets {
			fmt.Println(spcIndent + formatTaskSet(ts))
		}
	}

	if err := d.describeAutoScaling(s); err != nil {
		return nil, errors.Wrap(err, "failed to describe autoscaling")
	}

	fmt.Println("Events:")
	for i, event := range s.Events {
		if i >= events {
			break
		}
		for _, line := range formatEvent(event, TerminalWidth) {
			fmt.Println(line)
		}
	}
	return s, nil
}

func (d *App) describeAutoScaling(s *ecs.Service) error {
	resouceId := fmt.Sprintf("service/%s/%s", arnToName(*s.ClusterArn), *s.ServiceName)
	tout, err := d.autoScaling.DescribeScalableTargets(
		&applicationautoscaling.DescribeScalableTargetsInput{
			ResourceIds:       []*string{&resouceId},
			ServiceNamespace:  aws.String("ecs"),
			ScalableDimension: aws.String("ecs:service:DesiredCount"),
		},
	)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == "AccessDeniedException" {
				d.DebugLog("unable to describe scalable targets. requires IAM for application-autoscaling:Describe* to display informations about auto-scaling.")
				return nil
			}
		}
		return errors.Wrap(err, "failed to describe scalable targets")
	}
	if len(tout.ScalableTargets) == 0 {
		return nil
	}

	fmt.Println("AutoScaling:")
	for _, target := range tout.ScalableTargets {
		fmt.Println(formatScalableTarget(target))
	}

	pout, err := d.autoScaling.DescribeScalingPolicies(
		&applicationautoscaling.DescribeScalingPoliciesInput{
			ResourceId:        &resouceId,
			ServiceNamespace:  aws.String("ecs"),
			ScalableDimension: aws.String("ecs:service:DesiredCount"),
		},
	)
	if err != nil {
		return errors.Wrap(err, "failed to describe scaling policies")
	}
	for _, policy := range pout.ScalingPolicies {
		fmt.Println(formatScalingPolicy(policy))
	}
	return nil
}

func (d *App) DescribeServiceDeployments(ctx context.Context, startedAt time.Time) (int, error) {
	out, err := d.ecs.DescribeServicesWithContext(ctx, d.DescribeServicesInput())
	if err != nil {
		return 0, err
	}
	if len(out.Services) == 0 {
		return 0, nil
	}
	s := out.Services[0]
	lines := 0
	for _, dep := range s.Deployments {
		lines++
		d.Log(formatDeployment(dep))
	}
	for _, event := range s.Events {
		if (*event.CreatedAt).After(startedAt) {
			for _, line := range formatEvent(event, TerminalWidth) {
				fmt.Println(line)
				lines++
			}
		}
	}
	return lines, nil
}

func (d *App) DescribeTask(ctx context.Context, task *ecs.Task) error {
	out, err := d.ecs.DescribeTasksWithContext(ctx, d.DescribeTasksInput(task))
	if err != nil {
		return err
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		d.Log("Task ARN: " + *f.Arn)
		return errors.New(*f.Reason)
	}

	c := out.Tasks[0].Containers[0]
	if c.Reason != nil {
		return errors.New(*c.Reason)
	}
	if c.ExitCode != nil && *c.ExitCode != 0 {
		msg := "Exit Code: " + strconv.FormatInt(*c.ExitCode, 10)
		if c.Reason != nil {
			msg += ", Reason: " + *c.Reason
		}
		return errors.New(msg)
	}
	return nil
}

func (d *App) DescribeTaskDefinition(ctx context.Context, tdArn string) (*ecs.TaskDefinition, error) {
	out, err := d.ecs.DescribeTaskDefinitionWithContext(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: &tdArn,
	})
	if err != nil {
		return nil, err
	}
	return out.TaskDefinition, nil
}

func (d *App) GetLogEvents(ctx context.Context, logGroup string, logStream string, startedAt time.Time) (int, error) {
	ms := startedAt.UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
	out, err := d.cwl.GetLogEventsWithContext(ctx, d.GetLogEventsInput(logGroup, logStream, ms))
	if err != nil {
		return 0, err
	}
	if len(out.Events) == 0 {
		return 0, nil
	}
	lines := 0
	for _, event := range out.Events {
		for _, line := range formatLogEvent(event, TerminalWidth) {
			fmt.Println(line)
			lines++
		}
	}
	return lines, nil
}

func NewApp(conf *Config) (*App, error) {
	if err := conf.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid configuration")
	}
	sess := session.Must(session.NewSession(
		&aws.Config{Region: aws.String(conf.Region)},
	))
	d := &App{
		Service:     conf.Service,
		Cluster:     conf.Cluster,
		ecs:         ecs.New(sess),
		autoScaling: applicationautoscaling.New(sess),
		codedeploy:  codedeploy.New(sess),
		cwl:         cloudwatchlogs.New(sess),
		cwe:         cloudwatchevents.New(sess),
		config:      conf,
	}
	return d, nil
}

func (d *App) Start() (context.Context, context.CancelFunc) {
	log.SetOutput(os.Stdout)

	if d.config.Timeout > 0 {
		return context.WithTimeout(context.Background(), d.config.Timeout)
	} else {
		return context.Background(), func() {}
	}
}

func (d *App) Status(opt StatusOption) error {
	ctx, cancel := d.Start()
	defer cancel()
	_, err := d.DescribeServiceStatus(ctx, *opt.Events)
	return err
}

func (d *App) Create(opt CreateOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting create service", opt.DryRunString())
	svd, err := d.LoadServiceDefinition(d.config.ServiceDefinitionPath)
	if err != nil {
		return errors.Wrap(err, "failed to load service definition")
	}
	td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
	if err != nil {
		return errors.Wrap(err, "failed to load task definition")
	}

	if *opt.DesiredCount != 1 {
		svd.DesiredCount = opt.DesiredCount
	}

	if *opt.DryRun {
		d.Log("task definition:", td.String())
		d.Log("service definition:", svd.String())
		d.Log("DRY RUN OK")
		return nil
	}

	newTd, err := d.RegisterTaskDefinition(ctx, td)
	if err != nil {
		return errors.Wrap(err, "failed to register task definition")
	}
	svd.TaskDefinition = newTd.TaskDefinitionArn

	if _, err := d.ecs.CreateServiceWithContext(ctx, svd); err != nil {
		return errors.Wrap(err, "failed to create service")
	}
	d.Log("Service is created")

	if *opt.NoWait {
		return nil
	}

	start := time.Now()
	time.Sleep(delayForServiceChanged) // wait for service created
	if err := d.WaitServiceStable(ctx, start); err != nil {
		return errors.Wrap(err, "failed to wait service stable")
	}

	d.Log("Service is stable now. Completed!")
	return nil
}

func (d *App) Delete(opt DeleteOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Deleting service", opt.DryRunString())
	sv, err := d.DescribeServiceStatus(ctx, 3)
	if err != nil {
		return err
	}

	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	if !*opt.Force {
		service := prompter.Prompt(`Enter the service name to DELETE`, "")
		if service != *sv.ServiceName {
			d.Log("Aborted")
			return errors.New("confirmation failed")
		}
	}

	dsi := &ecs.DeleteServiceInput{
		Cluster: sv.ClusterArn,
		Service: sv.ServiceName,
	}
	if _, err := d.ecs.DeleteServiceWithContext(ctx, dsi); err != nil {
		return errors.Wrap(err, "failed to delete service")
	}
	d.Log("Service is deleted")

	return nil
}

func (d *App) Run(opt RunOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Running task", opt.DryRunString())
	var ov ecs.TaskOverride
	if ovStr := *opt.TaskOverrideStr; ovStr != "" {
		if err := json.Unmarshal([]byte(ovStr), &ov); err != nil {
			return errors.Wrap(err, "invalid overrides")
		}
	}

	sv, err := d.DescribeServiceStatus(ctx, 0)
	if err != nil {
		return errors.Wrap(err, "failed to describe service status")
	}

	var tdArn string
	var logConfiguration *ecs.LogConfiguration

	if *opt.SkipTaskDefinition {
		td, err := d.DescribeTaskDefinition(ctx, *sv.TaskDefinition)
		if err != nil {
			return errors.Wrap(err, "failed to describe task definition")
		}
		tdArn = *(td.TaskDefinitionArn)
		logConfiguration = td.ContainerDefinitions[0].LogConfiguration
		if *opt.DryRun {
			d.Log("task definition:", td.String())
		}
	} else {
		td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
		if err != nil {
			return errors.Wrap(err, "failed to load task definition")
		}

		if len(*opt.TaskDefinition) > 0 {
			d.Log("Loading task definition")
			runTd, err := d.LoadTaskDefinition(*opt.TaskDefinition)
			if err != nil {
				return errors.Wrap(err, "failed to load task definition")
			}
			td = runTd
		}

		var newTd *ecs.TaskDefinition
		_ = newTd

		if *opt.DryRun {
			d.Log("task definition:", td.String())
		} else {
			newTd, err = d.RegisterTaskDefinition(ctx, td)
			if err != nil {
				return errors.Wrap(err, "failed to register task definition")
			}
			tdArn = *newTd.TaskDefinitionArn
			logConfiguration = newTd.ContainerDefinitions[0].LogConfiguration
		}
	}
	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	task, err := d.RunTask(ctx, tdArn, sv, &ov, *opt.Count)
	if err != nil {
		return errors.Wrap(err, "failed to run task")
	}
	if *opt.NoWait {
		d.Log("Run task invoked")
		return nil
	}
	if err := d.WaitRunTask(ctx, task, logConfiguration, time.Now()); err != nil {
		return errors.Wrap(err, "failed to run task")
	}
	if err := d.DescribeTask(ctx, task); err != nil {
		return errors.Wrap(err, "failed to describe task")
	}
	d.Log("Run task completed!")

	return nil
}

func (d *App) Wait(opt WaitOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Waiting for the service stable")

	if err := d.WaitServiceStable(ctx, time.Now()); err != nil {
		return errors.Wrap(err, "the service still unstable")
	}

	d.Log("Service is stable now. Completed!")
	return nil
}

func (d *App) FindRollbackTarget(ctx context.Context, taskDefinitionArn string) (string, error) {
	var found bool
	var nextToken *string
	family := strings.Split(arnToName(taskDefinitionArn), ":")[0]
	for {
		out, err := d.ecs.ListTaskDefinitionsWithContext(ctx,
			&ecs.ListTaskDefinitionsInput{
				NextToken:    nextToken,
				FamilyPrefix: aws.String(family),
				MaxResults:   aws.Int64(100),
				Sort:         aws.String("DESC"),
			},
		)
		if err != nil {
			return "", errors.Wrap(err, "failed to list taskdefinitions")
		}
		if len(out.TaskDefinitionArns) == 0 {
			return "", errors.New("rollback target is not found")
		}
		nextToken = out.NextToken
		for _, tdArn := range out.TaskDefinitionArns {
			if found {
				return *tdArn, nil
			}
			if *tdArn == taskDefinitionArn {
				found = true
			}
		}
	}
}

func (d *App) Name() string {
	return fmt.Sprintf("%s/%s", d.Service, d.Cluster)
}

func (d *App) Log(v ...interface{}) {
	args := []interface{}{d.Name()}
	args = append(args, v...)
	log.Println(args...)
}

func (d *App) DebugLog(v ...interface{}) {
	if !d.Debug {
		return
	}
	d.Log(v...)
}

func (d *App) WaitServiceStable(ctx context.Context, startedAt time.Time) error {
	d.Log("Waiting for service stable...(it will take a few minutes)")
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		tick := time.Tick(10 * time.Second)
		var lines int
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-tick:
				if isTerminal {
					for i := 0; i < lines; i++ {
						fmt.Print(aec.EraseLine(aec.EraseModes.All), aec.PreviousLine(1))
					}
				}
				lines, _ = d.DescribeServiceDeployments(waitCtx, startedAt)
			}
		}
	}()

	// Add an option WithWaiterDelay and request.WithWaiterMaxAttempts for a long timeout.
	// SDK Default is 10 min (MaxAttempts=40 * Delay=15sec) at now.
	// ref. https://github.com/aws/aws-sdk-go/blob/d57c8d96f72d9475194ccf18d2ba70ac294b0cb3/service/ecs/waiters.go#L82-L83
	// Explicitly set these options so not being affected by the default setting.
	const delay = 15 * time.Second
	attempts := int((d.config.Timeout / delay)) + 1
	if (d.config.Timeout % delay) > 0 {
		attempts++
	}
	return d.ecs.WaitUntilServicesStableWithContext(
		ctx, d.DescribeServicesInput(),
		request.WithWaiterDelay(request.ConstantWaiterDelay(delay)),
		request.WithWaiterMaxAttempts(attempts),
	)
}

func (d *App) RegisterTaskDefinition(ctx context.Context, td *ecs.TaskDefinition) (*ecs.TaskDefinition, error) {
	d.Log("Registering a new task definition...")

	out, err := d.ecs.RegisterTaskDefinitionWithContext(
		ctx,
		&ecs.RegisterTaskDefinitionInput{
			ContainerDefinitions:    td.ContainerDefinitions,
			Cpu:                     td.Cpu,
			ExecutionRoleArn:        td.ExecutionRoleArn,
			Family:                  td.Family,
			Memory:                  td.Memory,
			NetworkMode:             td.NetworkMode,
			PlacementConstraints:    td.PlacementConstraints,
			RequiresCompatibilities: td.RequiresCompatibilities,
			TaskRoleArn:             td.TaskRoleArn,
			ProxyConfiguration:      td.ProxyConfiguration,
			Volumes:                 td.Volumes,
		},
	)
	if err != nil {
		return nil, err
	}
	d.Log("Task definition is registered", taskDefinitionName(out.TaskDefinition))
	return out.TaskDefinition, nil
}

func (d *App) LoadTaskDefinition(path string) (*ecs.TaskDefinition, error) {
	d.Log("Creating a new task definition by", path)
	c := struct {
		TaskDefinition *ecs.TaskDefinition
	}{}
	if err := config.LoadWithEnvJSON(&c, path); err != nil {
		return nil, err
	}
	if c.TaskDefinition != nil {
		return c.TaskDefinition, nil
	}
	var td ecs.TaskDefinition
	if err := config.LoadWithEnvJSON(&td, path); err != nil {
		return nil, err
	}
	return &td, nil
}

func (d *App) LoadServiceDefinition(path string) (*ecs.CreateServiceInput, error) {
	if path == "" {
		return nil, errors.New("service_definition is not defined")
	}

	c := ecs.CreateServiceInput{}
	if err := config.LoadWithEnvJSON(&c, path); err != nil {
		return nil, err
	}

	var count *int64
	if c.SchedulingStrategy == nil || *c.SchedulingStrategy == "REPLICA" && c.DesiredCount == nil {
		// set default desired count to 1 only when SchedulingStrategy is REPLICA(default)
		count = aws.Int64(1)
	} else if *c.SchedulingStrategy == "DAEMON" {
		count = nil
	} else {
		count = c.DesiredCount
	}

	c.Cluster = aws.String(d.config.Cluster)
	c.ServiceName = aws.String(d.config.Service)
	c.DesiredCount = count

	return &c, nil
}

func (d *App) GetLogInfo(task *ecs.Task, lc *ecs.LogConfiguration) (string, string) {
	taskId := strings.Split(*task.TaskArn, "/")[1]
	logStreamPrefix := *lc.Options["awslogs-stream-prefix"]
	containerName := *task.Containers[0].Name

	logStream := strings.Join([]string{logStreamPrefix, containerName, taskId}, "/")
	logGroup := *lc.Options["awslogs-group"]

	d.Log("logGroup:", logGroup)
	d.Log("logStream:", logStream)

	return logGroup, logStream
}

func (d *App) RunTask(ctx context.Context, tdArn string, sv *ecs.Service, ov *ecs.TaskOverride, count int64) (*ecs.Task, error) {
	d.Log("Running task")

	out, err := d.ecs.RunTaskWithContext(
		ctx,
		&ecs.RunTaskInput{
			Cluster:              aws.String(d.Cluster),
			TaskDefinition:       aws.String(tdArn),
			NetworkConfiguration: sv.NetworkConfiguration,
			LaunchType:           sv.LaunchType,
			Overrides:            ov,
			Count:                aws.Int64(count),
			CapacityProviderStrategy: sv.CapacityProviderStrategy,
			PlacementConstraints:     sv.PlacementConstraints,
			PlacementStrategy:        sv.PlacementStrategy,
			PlatformVersion:          sv.PlatformVersion,
		},
	)
	if err != nil {
		return nil, err
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		d.Log("Task ARN: " + *f.Arn)
		return nil, errors.New(*f.Reason)
	}

	task := out.Tasks[0]
	d.Log("Task ARN:", *task.TaskArn)
	return task, nil
}

func (d *App) WaitRunTask(ctx context.Context, task *ecs.Task, lc *ecs.LogConfiguration, startedAt time.Time) error {
	d.Log("Waiting for run task...(it may take a while)")
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if lc == nil || *lc.LogDriver != "awslogs" || lc.Options["awslogs-stream-prefix"] == nil {
		d.Log("awslogs not configured")
		return d.ecs.WaitUntilTasksStoppedWithContext(ctx, d.DescribeTasksInput(task))
	}

	logGroup, logStream := d.GetLogInfo(task, lc)
	time.Sleep(3 * time.Second) // wait for log stream

	go func() {
		tick := time.Tick(5 * time.Second)
		var lines int
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-tick:
				if isTerminal {
					for i := 0; i < lines; i++ {
						fmt.Print(aec.EraseLine(aec.EraseModes.All), aec.PreviousLine(1))
					}
				}
				lines, _ = d.GetLogEvents(waitCtx, logGroup, logStream, startedAt)
			}
		}
	}()
	return d.ecs.WaitUntilTasksStoppedWithContext(ctx, d.DescribeTasksInput(task))
}

func (d *App) Register(opt RegisterOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting register task definition", opt.DryRunString())
	td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
	if err != nil {
		return errors.Wrap(err, "failed to load task definition")
	}
	if *opt.DryRun {
		d.Log("task definition:", td.String())
		d.Log("DRY RUN OK")
		return nil
	}

	newTd, err := d.RegisterTaskDefinition(ctx, td)
	if err != nil {
		return errors.Wrap(err, "failed to register task definition")
	}

	if *opt.Output {
		fmt.Println(newTd.String())
	}
	return nil
}

func (d *App) suspendAutoScaling(suspend bool) error {
	resouceId := fmt.Sprintf("service/%s/%s", d.Cluster, d.Service)

	out, err := d.autoScaling.DescribeScalableTargets(
		&applicationautoscaling.DescribeScalableTargetsInput{
			ResourceIds:       []*string{&resouceId},
			ServiceNamespace:  aws.String("ecs"),
			ScalableDimension: aws.String("ecs:service:DesiredCount"),
		},
	)
	if err != nil {
		return errors.Wrap(err, "failed to describe scalable targets")
	}
	if len(out.ScalableTargets) == 0 {
		d.Log(fmt.Sprintf("No scalable target for %s", resouceId))
		return nil
	}
	for _, target := range out.ScalableTargets {
		d.Log(fmt.Sprintf("Register scalable target %s set suspend to %t", *target.ResourceId, suspend))
		_, err := d.autoScaling.RegisterScalableTarget(
			&applicationautoscaling.RegisterScalableTargetInput{
				ServiceNamespace:  target.ServiceNamespace,
				ScalableDimension: target.ScalableDimension,
				ResourceId:        target.ResourceId,
				SuspendedState: &applicationautoscaling.SuspendedState{
					DynamicScalingInSuspended:  &suspend,
					DynamicScalingOutSuspended: &suspend,
					ScheduledScalingSuspended:  &suspend,
				},
			},
		)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("failed to register scalable target %s set suspend to %t", *target.ResourceId, suspend))
		}
	}
	return nil
}

func (d *App) Schedule(opt ScheduleOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Schedule task", opt.DryRunString())
	var ov ecs.TaskOverride
	if ovStr := *opt.TaskOverrideStr; ovStr != "" {
		if err := json.Unmarshal([]byte(ovStr), &ov); err != nil {
			return errors.Wrap(err, "invalid overrides")
		}
	}

	sv, err := d.DescribeServiceStatus(ctx, 0)
	if err != nil {
		return errors.Wrap(err, "failed to describe service status")
	}

	var tdArn string

	if *opt.SkipTaskDefinition {
		td, err := d.DescribeTaskDefinition(ctx, *sv.TaskDefinition)
		if err != nil {
			return errors.Wrap(err, "failed to describe task definition")
		}
		tdArn = *(td.TaskDefinitionArn)
		if *opt.DryRun {
			d.Log("task definition:", td.String())
		}
	} else {
		td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
		if err != nil {
			return errors.Wrap(err, "failed to load task definition")
		}

		if len(*opt.TaskDefinition) > 0 {
			d.Log("Loading task definition")
			runTd, err := d.LoadTaskDefinition(*opt.TaskDefinition)
			if err != nil {
				return errors.Wrap(err, "failed to load task definition")
			}
			td = runTd
		}

		var newTd *ecs.TaskDefinition
		_ = newTd

		if *opt.DryRun {
			d.Log("task definition:", td.String())
		} else {
			newTd, err = d.RegisterTaskDefinition(ctx, td)
			if err != nil {
				return errors.Wrap(err, "failed to register task definition")
			}
			tdArn = *newTd.TaskDefinitionArn
		}
	}

	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	err = d.ScheduleTask(ctx, tdArn, *opt.RoleArn, sv, &ov, *opt.EventName, *opt.ScheduleExpression)
	if err != nil {
		return err
	}

	d.Log("Scheduled task!")

	return nil
}

func toCweNetworkConfiguration(nc *ecs.NetworkConfiguration) *cloudwatchevents.NetworkConfiguration {
	if nc == nil {
		return nil
	}

	var vpcConf *cloudwatchevents.AwsVpcConfiguration
	if nc.AwsvpcConfiguration != nil {
		vpcConf = &cloudwatchevents.AwsVpcConfiguration{
			AssignPublicIp: nc.AwsvpcConfiguration.AssignPublicIp,
			SecurityGroups: nc.AwsvpcConfiguration.SecurityGroups,
			Subnets:        nc.AwsvpcConfiguration.Subnets,
		}
	}
	return &cloudwatchevents.NetworkConfiguration{
		AwsvpcConfiguration: vpcConf,
	}
}

func (d *App) ScheduleTask(ctx context.Context, tdArn string, roleArn string, sv *ecs.Service, ov *ecs.TaskOverride, name string, scheduleExp string) error {
	in := &cloudwatchevents.PutRuleInput{
		Name:               aws.String(name),
		ScheduleExpression: aws.String(scheduleExp),
	}
	rule, err := d.cwe.PutRuleWithContext(ctx, in)
	if err != nil {
		return err
	}

	d.Log("Rule ARN:", *rule.RuleArn)

	var input *string
	if ov != nil {
		j, err := MarshalJSON(ov)
		if err != nil {
			return err
		}
		input = aws.String(string(j))
	}
	t := &cloudwatchevents.PutTargetsInput{
		Rule: aws.String(name),
		Targets: []*cloudwatchevents.Target{
			&cloudwatchevents.Target{
				Arn:     sv.ClusterArn,
				RoleArn: aws.String(roleArn),
				Id:      aws.String(fmt.Sprintf("%v-ecs", name)),
				EcsParameters: &cloudwatchevents.EcsParameters{
					LaunchType:           sv.LaunchType,
					NetworkConfiguration: toCweNetworkConfiguration(sv.NetworkConfiguration),
					TaskDefinitionArn:    aws.String(tdArn),
					TaskCount:            aws.Int64(1),
				},
				Input: input,
			},
		},
	}
	_, err = d.cwe.PutTargetsWithContext(ctx, t)

	if err != nil {
		return err
	}
	return nil
}
