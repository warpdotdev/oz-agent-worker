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

func newTokenSource(ctx context.Context, serverRootURL, runID string, env map[string]string) oauth2.TokenSource {
	return oauth2.ReuseTokenSourceWithExpiry(nil, &taskIdentitySource{
		// Final flush may need a fresh token after the reporter context is cancelled.
		ctx:      context.WithoutCancel(ctx),
		client:   &http.Client{Timeout: exportTimeout, CheckRedirect: sameOriginRedirect},
		endpoint: strings.TrimRight(serverRootURL, "/") + "/graphql/v2",
		runID:    runID, apiKey: env["WARP_API_KEY"], workloadToken: env["WARP_WORKLOAD_TOKEN"],
	}, time.Minute)
}

func (s *taskIdentitySource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(s.ctx, exportTimeout)
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
