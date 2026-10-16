package otlp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const ExportTimeout = 5 * time.Second

func Endpoint(raw, signal string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", errors.New("invalid task " + signal + " collector endpoint")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/" + signal
	return endpoint.String(), nil
}

func ValidateIdentityEndpoint(raw string) error {
	server, err := url.Parse(raw)
	if err != nil || server.Host == "" || (server.Scheme != "https" && server.Scheme != "http") || server.User != nil || server.RawQuery != "" || server.Fragment != "" {
		return errors.New("invalid task identity endpoint")
	}
	return nil
}

func Client(ctx context.Context, source oauth2.TokenSource) *http.Client {
	base := &http.Client{Timeout: ExportTimeout, CheckRedirect: SameOriginRedirect}
	return oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, base), source)
}

func SameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	// OAuth2 reattaches bearer credentials on every hop, even across origins.
	origin := via[0].URL
	if req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Host, origin.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}
