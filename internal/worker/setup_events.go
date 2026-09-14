package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

const (
	warpAPIKeyEnv        = "WARP_API_KEY"
	warpWorkloadTokenEnv = "WARP_WORKLOAD_TOKEN" // #nosec G101 -- environment variable name, not a credential

	cloudAgentIDHeader  = "X-Warp-Cloud-Agent-ID"
	workloadTokenHeader = "X-Warp-Ambient-Workload-Token" // #nosec G101 -- header name, not a credential

	// Worker-observed setup phases reported as run client events. warp-server
	// ingests them as setup metrics, labeled with the run's current timeline
	// phase (oz_run_claimed while the container is still being prepared).
	// SetupEventContainerStart spans container creation through a successful
	// start call on the Docker backend, and the span from the end of init
	// (or scheduling) to the task container start on the Kubernetes backend.
	SetupEventImagePull      = "setup_worker_image_pull"
	SetupEventSidecarPrep    = "setup_worker_sidecar_prep"
	SetupEventContainerStart = "setup_worker_container_start"

	// Kubernetes-backend phases. SetupEventJobCreate is timed directly around
	// the Job create call; the other phases are derived from pod status by
	// kubernetesSetupPhaseTracker.
	SetupEventJobCreate    = "setup_worker_job_create"
	SetupEventPodSchedule  = "setup_worker_pod_schedule"
	SetupEventSetupCommand = "setup_worker_setup_command"

	setupEventRequestTimeout = 5 * time.Second
)

// setupEventReporter reports worker-observed task setup phase durations to
// warp-server's run client-events endpoint. Reporting is best-effort: failures
// are logged and never affect task execution.
type setupEventReporter struct {
	httpClient    *http.Client
	serverRootURL string
	runID         string
	apiKey        string
	workloadToken string
}

// newSetupEventReporter builds a reporter from the worker's server root URL and
// the task assignment's credentials. It returns nil (a valid no-op reporter)
// when the server root URL or the per-task API key is unavailable.
func newSetupEventReporter(serverRootURL string, assignment *types.TaskAssignmentMessage) *setupEventReporter {
	if serverRootURL == "" || assignment == nil {
		return nil
	}
	apiKey := assignment.EnvVars[warpAPIKeyEnv]
	if apiKey == "" {
		return nil
	}
	return &setupEventReporter{
		httpClient:    &http.Client{Timeout: setupEventRequestTimeout},
		serverRootURL: strings.TrimRight(serverRootURL, "/"),
		runID:         assignment.TaskID,
		apiKey:        apiKey,
		workloadToken: assignment.EnvVars[warpWorkloadTokenEnv],
	}
}

// startPhase begins timing one setup phase and returns a completion func that
// reports the phase result. See startPhaseIf for the reporting rules.
func (r *setupEventReporter) startPhase(ctx context.Context, eventName string) func(isError bool) {
	return r.startPhaseIf(ctx, eventName, true)
}

// startPhaseIf begins timing one setup phase and returns a completion func
// that reports the phase with the elapsed wall-clock time. The func skips the
// report when shouldReport is false, and when the context was cancelled: a
// cancelled phase is neither a success nor a failure, so recording it would
// skew both the duration and the failure-rate metrics. Both funcs are safe to
// call on a nil reporter.
func (r *setupEventReporter) startPhaseIf(ctx context.Context, eventName string, shouldReport bool) func(isError bool) {
	start := time.Now()
	return func(isError bool) {
		if r == nil || !shouldReport || ctx.Err() != nil {
			return
		}
		r.reportPhase(ctx, eventName, start, time.Now(), isError)
	}
}

// reportPhase asynchronously reports one completed setup phase. It is safe to
// call on a nil reporter and never blocks task execution.
func (r *setupEventReporter) reportPhase(ctx context.Context, eventName string, start, finish time.Time, isError bool) {
	if r == nil {
		return
	}
	// Detach from the task context so cancellation of the task (including the
	// failure that is being reported) does not drop the report.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), setupEventRequestTimeout)
	go func() {
		defer cancel()
		if err := r.send(sendCtx, eventName, start, finish, isError); err != nil {
			log.Warnf(sendCtx, "Failed to report setup event %s for task %s: %v", eventName, r.runID, err)
		}
	}()
}

type setupMetricPayload struct {
	StartTS   time.Time `json:"start_ts"`
	FinishTS  time.Time `json:"finish_ts"`
	LatencyMS float64   `json:"latency_ms"`
	IsError   bool      `json:"is_error"`
}

type clientEventRequest struct {
	EventUUID string             `json:"event_uuid"`
	EventName string             `json:"event_name"`
	Timestamp time.Time          `json:"timestamp"`
	Payload   setupMetricPayload `json:"payload"`
}

// send synchronously posts one setup event to the run client-events endpoint.
func (r *setupEventReporter) send(ctx context.Context, eventName string, start, finish time.Time, isError bool) error {
	body, err := json.Marshal(clientEventRequest{
		EventUUID: uuid.NewString(),
		EventName: eventName,
		Timestamp: finish,
		Payload: setupMetricPayload{
			StartTS:   start,
			FinishTS:  finish,
			LatencyMS: float64(finish.Sub(start).Milliseconds()),
			IsError:   isError,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal client event: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/agent/runs/%s/client-events", r.serverRootURL, url.PathEscape(r.runID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build client event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set(cloudAgentIDHeader, r.runID)
	if r.workloadToken != "" {
		req.Header.Set(workloadTokenHeader, r.workloadToken)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Warnf(ctx, "Failed to close client event response body: %v", closeErr)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("client event request returned status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
