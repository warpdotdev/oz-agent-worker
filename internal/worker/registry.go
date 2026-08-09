package worker

import (
	"sync"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
)

// executionKey identifies one execution exactly. A run can be executed more
// than once (follow-ups, handoffs), so debug-archive ownership is only ever
// resolved through both identifiers.
type executionKey struct {
	runID       string
	executionID string
}

// ownedExecution is the worker's claim over one execution: the backend that
// ran it and the direct-backend capture, if any.
type ownedExecution struct {
	backendKind string
	capture     *debuglog.TaskLogCapture
	// cleanupTimer fires backend resource cleanup at the grace deadline. It is
	// nil while the execution is still active, and is only ever touched under
	// the registry's mutex: a zero grace fires the timer before the arming call
	// returns, so its callback races an unguarded field.
	cleanupTimer *time.Timer
}

// TaskRegistry tracks the worker's in-progress tasks. It answers two
// questions: which task ID a cancellation should route to, and whether this
// process executed one exact (run, execution) pair — the ownership test that
// makes only the executing instance answer a debug-archive log request.
type TaskRegistry struct {
	mu sync.Mutex
	// active is keyed by task ID because cancellation messages carry only
	// task_id.
	active map[string]activeTask
	// owned and grace are keyed by the exact execution identity a
	// debug-archive request names.
	owned map[executionKey]*ownedExecution
	grace map[executionKey]*ownedExecution
}

func newTaskRegistry() *TaskRegistry {
	return &TaskRegistry{
		active: make(map[string]activeTask),
		owned:  make(map[executionKey]*ownedExecution),
		grace:  make(map[executionKey]*ownedExecution),
	}
}

// StartTask records an assignment as active and claims ownership of its exact
// execution identity.
func (r *TaskRegistry) StartTask(taskID string, task activeTask, backendKind string, capture *debuglog.TaskLogCapture) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.active[taskID] = task
	r.owned[executionKey{runID: taskID, executionID: task.executionID}] = &ownedExecution{
		backendKind: backendKind,
		capture:     capture,
	}
}

// Get returns the active task for a task ID.
func (r *TaskRegistry) Get(taskID string) (activeTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.active[taskID]
	return task, ok
}

// Update replaces an active task's record when it is still tracked.
func (r *TaskRegistry) Update(taskID string, mutate func(*activeTask)) (activeTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	task, ok := r.active[taskID]
	if !ok {
		return activeTask{}, false
	}
	mutate(&task)
	r.active[taskID] = task
	return task, true
}

// Delete stops tracking an active task. Any cleanup-grace entry survives, so a
// terminal execution stays reachable for log collection; only an execution
// that never reached cleanup grace loses its ownership here.
func (r *TaskRegistry) Delete(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	task, ok := r.active[taskID]
	if !ok {
		return
	}
	delete(r.active, taskID)
	delete(r.owned, executionKey{runID: taskID, executionID: task.executionID})
}

// Len reports how many tasks are active.
func (r *TaskRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// Snapshot copies the active task map for iteration outside the lock.
func (r *TaskRegistry) Snapshot() map[string]activeTask {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]activeTask, len(r.active))
	for taskID, task := range r.active {
		out[taskID] = task
	}
	return out
}

// LookupExecution implements debuglog.OwnershipLookup. A miss means this
// process did not execute the assignment, so the caller must stay silent.
func (r *TaskRegistry) LookupExecution(runID, executionID string) (debuglog.Ownership, bool) {
	key := executionKey{runID: runID, executionID: executionID}

	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.owned[key]; ok {
		return debuglog.Ownership{BackendKind: entry.backendKind, Capture: entry.capture}, true
	}
	if entry, ok := r.grace[key]; ok {
		return debuglog.Ownership{
			BackendKind:    entry.backendKind,
			InCleanupGrace: true,
			Capture:        entry.capture,
		}, true
	}
	return debuglog.Ownership{}, false
}

// MoveToCleanupGrace transfers an execution from active to cleanup-grace
// ownership and schedules onExpiry at the deadline. It runs before the terminal
// lifecycle message is enqueued, so a request the server triggers off that
// message always finds the grace entry rather than racing its deletion.
func (r *TaskRegistry) MoveToCleanupGrace(runID, executionID string, grace time.Duration, onExpiry func()) {
	key := executionKey{runID: runID, executionID: executionID}

	r.mu.Lock()
	entry, ok := r.owned[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.owned, key)
	r.grace[key] = entry
	// The timer is armed while the lock is held because a zero grace fires
	// onExpiry immediately; its callback blocks on this same lock until the
	// entry is fully published.
	entry.cleanupTimer = time.AfterFunc(grace, onExpiry)
	graceCount := len(r.grace)
	r.mu.Unlock()

	metrics.SetCleanupGraceEntries(graceCount)
}

// ReleaseCleanupGrace drops an execution's cleanup-grace entry. It is
// idempotent so an expiry timer and a shutdown sweep can both call it.
func (r *TaskRegistry) ReleaseCleanupGrace(runID, executionID string) (*ownedExecution, bool) {
	key := executionKey{runID: runID, executionID: executionID}

	r.mu.Lock()
	entry, ok := r.grace[key]
	if ok {
		delete(r.grace, key)
		if entry.cleanupTimer != nil {
			entry.cleanupTimer.Stop()
		}
	}
	graceCount := len(r.grace)
	r.mu.Unlock()

	if !ok {
		return nil, false
	}
	metrics.SetCleanupGraceEntries(graceCount)
	return entry, true
}

// ReleaseOwnership drops an execution's active ownership without moving it to
// cleanup grace. It covers the paths where no terminal state was ever reached,
// such as a rejected assignment.
func (r *TaskRegistry) ReleaseOwnership(runID, executionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.owned, executionKey{runID: runID, executionID: executionID})
}

// PendingCleanups returns every cleanup-grace entry and clears the registry's
// record of them, so shutdown can release their resources exactly once.
func (r *TaskRegistry) PendingCleanups() map[executionKey]*ownedExecution {
	r.mu.Lock()
	entries := r.grace
	r.grace = make(map[executionKey]*ownedExecution)
	for _, entry := range entries {
		if entry.cleanupTimer != nil {
			entry.cleanupTimer.Stop()
		}
	}
	r.mu.Unlock()

	metrics.SetCleanupGraceEntries(0)
	return entries
}
