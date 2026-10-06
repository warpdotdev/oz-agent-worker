package tasklogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"
)

const identityMutation = `mutation WorkerLogToken($input: IssueTaskIdentityTokenInput!) {
  issueTaskIdentityToken(requestContext: {osContext: {}, clientContext: {}}, input: $input) {
    __typename
    ... on IssueTaskIdentityTokenOutput { token expiresAt }
  }
}`

type tokenSource struct {
	client                                 *http.Client
	endpoint, runID, apiKey, workloadToken string
	gate                                   *semaphore.Weighted
	token                                  string
	expiresAt                              time.Time
	refreshAt                              time.Time
	redactor                               *redactor
	done                                   chan struct{}
}

func newTokenSource(serverRootURL, runID string, env map[string]string, redactor *redactor) *tokenSource {
	return &tokenSource{
		client:   &http.Client{Timeout: exportTimeout, CheckRedirect: noRedirect},
		endpoint: strings.TrimRight(serverRootURL, "/") + "/graphql/v2",
		runID:    runID, apiKey: env["WARP_API_KEY"], workloadToken: env["WARP_WORKLOAD_TOKEN"],
		gate: semaphore.NewWeighted(1), redactor: redactor, done: make(chan struct{}),
	}
}

func (s *tokenSource) get(ctx context.Context) (string, time.Time, error) {
	if err := s.gate.Acquire(ctx, 1); err != nil {
		return "", time.Time{}, err
	}
	defer s.gate.Release(1)
	if time.Now().Before(s.refreshAt) {
		return s.token, s.refreshAt, nil
	}
	token, expiry, err := s.issue(ctx)
	if err != nil {
		// A failed proactive refresh must not discard a still-valid credential.
		if time.Now().Before(s.expiresAt) {
			s.refreshAt = s.expiresAt
			if retryAt := time.Now().Add(30 * time.Second); retryAt.Before(s.refreshAt) {
				s.refreshAt = retryAt
			}
			return s.token, s.refreshAt, nil
		}
		return "", time.Time{}, err
	}
	s.redactor.add(token)
	s.token, s.expiresAt = token, expiry
	margin := min(time.Minute, time.Until(expiry)/5)
	s.refreshAt = expiry.Add(-margin)
	return s.token, s.refreshAt, nil
}

func (s *tokenSource) run(ctx context.Context) {
	defer close(s.done)
	for {
		requestCtx, cancel := context.WithTimeout(ctx, exportTimeout)
		_, refreshAt, err := s.get(requestCtx)
		cancel()
		delay := time.Until(refreshAt)
		if err != nil {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(max(delay, time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *tokenSource) issue(ctx context.Context) (string, time.Time, error) {
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

type tokenTransport struct {
	source *tokenSource
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, _, err := t.source.get(req.Context())
	if err != nil {
		return nil, err
	}
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultTransport.RoundTrip(copy)
}
