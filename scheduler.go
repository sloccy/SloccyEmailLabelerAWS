package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

// scanScheduler rewrites the EventBridge Scheduler schedule that triggers the ScanFunction.
// When the schedule env vars are unset (e.g. local dev) client is nil and UpdateInterval is a
// no-op, so the web UI still works without AWS.
type scanScheduler struct {
	client  *scheduler.Client // nil = disabled (not configured)
	name    string            // AWS::Scheduler::Schedule name
	target  string            // ScanFunction ARN
	roleArn string            // role Scheduler assumes to invoke the target
}

// newScanScheduler builds a scanScheduler from config. When the schedule is not configured it
// returns a disabled scheduler (client nil) whose UpdateInterval is a safe no-op.
func newScanScheduler(ctx context.Context, cfg Config) (*scanScheduler, error) {
	s := &scanScheduler{
		name:    cfg.ScanScheduleName,
		target:  cfg.ScanFunctionArn,
		roleArn: cfg.SchedulerRoleArn,
	}
	if s.name == "" || s.target == "" || s.roleArn == "" {
		return s, nil // disabled
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	s.client = scheduler.NewFromConfig(awsCfg)
	return s, nil
}

// UpdateInterval sets the schedule to fire every `minutes` minutes. UpdateSchedule is a full
// replace, so the target/role/window must be re-supplied on every call.
func (s *scanScheduler) UpdateInterval(ctx context.Context, minutes int) error {
	if s == nil || s.client == nil {
		return nil
	}
	_, err := s.client.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{
		Name:               aws.String(s.name),
		ScheduleExpression: aws.String(rateExpr(minutes)),
		FlexibleTimeWindow: &types.FlexibleTimeWindow{Mode: types.FlexibleTimeWindowModeOff},
		State:              types.ScheduleStateEnabled,
		Target: &types.Target{
			Arn:     aws.String(s.target),
			RoleArn: aws.String(s.roleArn),
		},
	})
	if err != nil {
		return fmt.Errorf("update schedule %q: %w", s.name, err)
	}
	return nil
}

// rateExpr formats an EventBridge rate() expression, honouring the singular "minute" for 1.
func rateExpr(minutes int) string {
	if minutes == 1 {
		return "rate(1 minute)"
	}
	return fmt.Sprintf("rate(%d minutes)", minutes)
}

// cadenceLabel is the human-readable schedule shown in the UI (sidebar + dashboard).
func cadenceLabel(minutes int) string {
	switch {
	case minutes <= 1:
		return "1 min"
	case minutes < 60 || minutes%60 != 0:
		return fmt.Sprintf("%d min", minutes)
	case minutes == 60:
		return "1 hr"
	default:
		return fmt.Sprintf("%d hr", minutes/60)
	}
}
