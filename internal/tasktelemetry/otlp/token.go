package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"golang.org/x/oauth2"
)

const identityMutation = `mutation WorkerLogToken($input: IssueTaskIdentityTokenInput!) {
  issueTaskIdentityToken(requestContext: {osContext: {}, clientContext: {}}, input: $input) {
    __typename
    ... on IssueTaskIdentityTokenOutput { token expiresAt }
  }
}`

type taskIdentitySource struct {
	ctx                                    context.Context
	client                                 *http.Client
	endpoint, runID, apiKey, workloadToken string
}

type RefreshConfig struct {
	ServerRootURL string
	RunID         string
	APIKey        string
	WorkloadToken string
}

func BootstrapToken(assignment *types.TaskAssignmentMessage) *oauth2.Token {
	if assignment == nil || assignment.TelemetryCollection == nil {
		return nil
	}
	if bootstrap := assignment.TelemetryCollection.BootstrapToken; bootstrap != nil {
		expiry, err := time.Parse(time.RFC3339Nano, bootstrap.ExpiresAt)
		if err == nil && bootstrap.Token != "" && expiry.After(time.Now()) {
			return &oauth2.Token{AccessToken: bootstrap.Token, TokenType: "Bearer", Expiry: expiry}
		}
	}
	return nil
}
func RefreshFromAssignment(serverRootURL string, assignment *types.TaskAssignmentMessage) RefreshConfig {
	config := RefreshConfig{ServerRootURL: serverRootURL}
	if assignment != nil {
		config.RunID = assignment.TaskID
		config.APIKey = assignment.EnvVars["WARP_API_KEY"]
		config.WorkloadToken = assignment.EnvVars["WARP_WORKLOAD_TOKEN"]
	}
	return config
}

func NewTokenSource(ctx context.Context, bootstrap *oauth2.Token, refresh RefreshConfig) (oauth2.TokenSource, error) {
	if refresh.APIKey != "" && refresh.WorkloadToken != "" {
		if err := ValidateIdentityEndpoint(refresh.ServerRootURL); err != nil {
			return nil, err
		}
	} else if bootstrap == nil {
		return nil, errors.New("task telemetry authentication unavailable")
	}
	return oauth2.ReuseTokenSourceWithExpiry(bootstrap, &taskIdentitySource{
		ctx:      ctx,
		client:   &http.Client{Timeout: ExportTimeout, CheckRedirect: SameOriginRedirect},
		endpoint: strings.TrimRight(refresh.ServerRootURL, "/") + "/graphql/v2",
		runID:    refresh.RunID, apiKey: refresh.APIKey, workloadToken: refresh.WorkloadToken,
	}, time.Minute), nil
}

func (s *taskIdentitySource) Token() (*oauth2.Token, error) {
	if s.apiKey == "" || s.workloadToken == "" {
		return nil, errors.New("task identity credentials unavailable")
	}
	ctx, cancel := context.WithTimeout(s.ctx, ExportTimeout)
	defer cancel()
	token, expiry, err := s.issue(ctx)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{AccessToken: token, TokenType: "Bearer", Expiry: expiry}, nil
}

func (s *taskIdentitySource) issue(ctx context.Context) (string, time.Time, error) {
	body, err := json.Marshal(map[string]any{
		"query": identityMutation,
		"variables": map[string]any{"input": map[string]any{
			"audience":                 "warp-cloud-agent-otel",
			"requestedDurationSeconds": 10800,
		}},
	})
	if err != nil {
		return "", time.Time{}, errors.New("could not encode task identity request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, errors.New("could not build task identity request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("X-Warp-Cloud-Agent-ID", s.runID)
	req.Header.Set("X-Warp-Ambient-Workload-Token", s.workloadToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", time.Time{}, errors.New("task identity request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, errors.New("task identity request rejected")
	}
	var response struct {
		Data struct {
			Result struct {
				Type      string    `json:"__typename"`
				Token     string    `json:"token"`
				ExpiresAt time.Time `json:"expiresAt"`
			} `json:"issueTaskIdentityToken"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&response) != nil {
		return "", time.Time{}, errors.New("invalid task identity response")
	}
	result := response.Data.Result
	if len(response.Errors) != 0 || result.Type != "IssueTaskIdentityTokenOutput" || result.Token == "" || !result.ExpiresAt.After(time.Now()) {
		return "", time.Time{}, errors.New("task identity token unavailable")
	}
	return result.Token, result.ExpiresAt, nil
}
