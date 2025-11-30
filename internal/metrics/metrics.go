// internal/metrics/metrics.go
package metrics

import (
	"sync"
	"time"
)

// ActionsMetrics tracks live operation metrics for user-visible progress via /metrics.
type ActionsMetrics struct {
	mu sync.RWMutex

	// High-level operation context
	Operation  string    // e.g., "scale-up", "scale-down", "rolling-restart", "sync-state", "verify"
	Phase      string    // e.g., "planning", "launch", "terminate", "verify", "done"
	StartedAt  time.Time // operation start time
	LastUpdate time.Time

	// Scale-up counters
	LaunchRequested int
	LaunchSucceeded int
	LaunchFailed    int

	// Scale-down counters
	TerminateRequested int
	TerminateSucceeded int
	TerminateFailed    int

	// Rolling restart book-keeping
	RollingRestartIndex int // 1-based index of current item
	RollingRestartTotal int

	// Load Balancer snapshot
	LbEnabled  bool
	LbId       string
	LbBackends int

	// Scale target context
	TargetTotal int
	StartTotal  int

	// Last error encountered (if any)
	LastError string
}

var global = &ActionsMetrics{}

// Reset initializes/overwrites the current operation and clears counters.
func Reset(operation string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.Operation = operation
	global.Phase = "planning"
	now := time.Now()
	global.StartedAt = now
	global.LastUpdate = now
	global.TargetTotal = 0
	global.StartTotal = 0

	global.LaunchRequested = 0
	global.LaunchSucceeded = 0
	global.LaunchFailed = 0

	global.TerminateRequested = 0
	global.TerminateSucceeded = 0
	global.TerminateFailed = 0

	global.RollingRestartIndex = 0
	global.RollingRestartTotal = 0

	global.LastError = ""
}

// Done marks the current operation as completed.
func Done() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.Phase = "done"
	// Clear operation so UI badge shows "Scaling idle" until next Reset()
	global.Operation = ""
	global.LastUpdate = time.Now()
}

// SetPhase updates the current phase.
func SetPhase(phase string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.Phase = phase
	global.LastUpdate = time.Now()
}

// SetError records the last error string.
func SetError(err string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.LastError = err
	global.LastUpdate = time.Now()
}

// SetScaleTargets sets the starting and target totals for a scale operation.
func SetScaleTargets(start, target int) {
	global.mu.Lock()
	defer global.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if target < 0 {
		target = 0
	}
	global.StartTotal = start
	global.TargetTotal = target
	global.LastUpdate = time.Now()
}

// IncLaunchRequested increments the number of launches requested by n (can be negative to correct).
func IncLaunchRequested(n int) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.LaunchRequested += n
	if global.LaunchRequested < 0 {
		global.LaunchRequested = 0
	}
	global.LastUpdate = time.Now()
}

// IncLaunchSucceeded increments the number of successful launches by 1.
func IncLaunchSucceeded() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.LaunchSucceeded++
	global.LastUpdate = time.Now()
}

// IncLaunchFailed increments the number of failed launches by 1 and records err (optional).
func IncLaunchFailed(err string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.LaunchFailed++
	if err != "" {
		global.LastError = err
	}
	global.LastUpdate = time.Now()
}

// IncTerminateRequested increments the number of terminations requested by n (can be negative to correct).
func IncTerminateRequested(n int) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.TerminateRequested += n
	if global.TerminateRequested < 0 {
		global.TerminateRequested = 0
	}
	global.LastUpdate = time.Now()
}

// IncTerminateSucceeded increments the number of successful terminations by 1.
func IncTerminateSucceeded() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.TerminateSucceeded++
	global.LastUpdate = time.Now()
}

// IncTerminateFailed increments the number of failed terminations by 1 and records err (optional).
func IncTerminateFailed(err string) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.TerminateFailed++
	if err != "" {
		global.LastError = err
	}
	global.LastUpdate = time.Now()
}

// SetRollingRestart sets current index (1-based) and total items for rolling restart.
func SetRollingRestart(index, total int) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.RollingRestartIndex = index
	global.RollingRestartTotal = total
	global.LastUpdate = time.Now()
}

// UpdateLB sets the load balancer snapshot fields.
func UpdateLB(enabled bool, id string, backends int) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.LbEnabled = enabled
	global.LbId = id
	if backends < 0 {
		backends = 0
	}
	global.LbBackends = backends
	global.LastUpdate = time.Now()
}

// SetLBBackends updates just the backend count (e.g., during reconcile).
func SetLBBackends(n int) {
	global.mu.Lock()
	defer global.mu.Unlock()
	if n < 0 {
		n = 0
	}
	global.LbBackends = n
	global.LastUpdate = time.Now()
}

// Snapshot returns a copy of current metrics suitable for JSON encoding.
func Snapshot() map[string]any {
	global.mu.RLock()
	defer global.mu.RUnlock()
	out := map[string]any{
		"operation":           global.Operation,
		"phase":               global.Phase,
		"startedAt":           global.StartedAt.Format(time.RFC3339),
		"lastUpdate":          global.LastUpdate.Format(time.RFC3339),
		"launchRequested":     global.LaunchRequested,
		"launchSucceeded":     global.LaunchSucceeded,
		"launchFailed":        global.LaunchFailed,
		"terminateRequested":  global.TerminateRequested,
		"terminateSucceeded":  global.TerminateSucceeded,
		"terminateFailed":     global.TerminateFailed,
		"rollingRestartIndex": global.RollingRestartIndex,
		"rollingRestartTotal": global.RollingRestartTotal,
		"lbEnabled":           global.LbEnabled,
		"lbId":                global.LbId,
		"lbBackends":          global.LbBackends,
		"startTotal":          global.StartTotal,
		"targetTotal":         global.TargetTotal,
		"lastError":           global.LastError,
	}
	return out
}
