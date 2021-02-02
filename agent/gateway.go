package agent

import (
	"context"
	"time"

	"github.com/open-policy-agent/frameworks/constraint/pkg/types"
)

type AutomationHandler func(automation *Automation) error
type RestartHandler func() error
type ChangeLogLevelHandler func(level *LogLevel) error

type Gateway interface {
	Start(ctx context.Context) error
	WaitAuthorization(timeout time.Duration) error
	// TODO: Add Sync() function to ensure all buffered data is sent before exit

	SendMetrics(metrics []*Metric) error
	SendEntitiesDeltas(deltas []*Delta) error
	SendEntitiesResync(resync *EntitiesResync) error
	SendAutomationFeedback(feedback *AutomationFeedback) error

	SetAutomationHandler(handler AutomationHandler)
	SetRestartHandler(handler RestartHandler)
	SetChangeLogLevelHandler(handler ChangeLogLevelHandler)
	SendRecs(recommendations []*types.Result) error
}
