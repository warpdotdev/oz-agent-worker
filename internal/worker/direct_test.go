package worker

import (
	"os"
	"strings"
	"testing"
)

func TestHostBaseEnvIncludesProxyVariables(t *testing.T) {
	t.Setenv("HOME", "/home/agent")
	t.Setenv("HTTP_PROXY", "http://proxy:80")
	t.Setenv("HTTPS_PROXY", "http://proxy:80")
	t.Setenv("NO_PROXY", "localhost,.svc.cluster.local")
	t.Setenv("http_proxy", "http://proxy:80")
	t.Setenv("https_proxy", "http://proxy:80")
	t.Setenv("no_proxy", "localhost,.svc.cluster.local")

	env := hostBaseEnv()

	expected := map[string]string{
		"HOME=/home/agent":                              "HOME should be inherited",
		"HTTP_PROXY=http://proxy:80":                    "HTTP_PROXY should be inherited",
		"HTTPS_PROXY=http://proxy:80":                   "HTTPS_PROXY should be inherited",
		"NO_PROXY=localhost,.svc.cluster.local":         "NO_PROXY should be inherited",
		"http_proxy=http://proxy:80":                    "http_proxy should be inherited",
		"https_proxy=http://proxy:80":                   "https_proxy should be inherited",
		"no_proxy=localhost,.svc.cluster.local":         "no_proxy should be inherited",
	}

	for entry, msg := range expected {
		if !containsEnvEntry(env, entry) {
			t.Fatalf("%s; got env=%v", msg, env)
		}
	}
}

func containsEnvEntry(env []string, want string) bool {
	for _, entry := range env {
		if strings.EqualFold(entry, want) || entry == want {
			return true
		}
	}
	return false
}

func TestHostBaseEnvOmitsUnsetVariables(t *testing.T) {
	_ = os.Unsetenv("HTTP_PROXY")
	_ = os.Unsetenv("HTTPS_PROXY")
	_ = os.Unsetenv("NO_PROXY")
	_ = os.Unsetenv("http_proxy")
	_ = os.Unsetenv("https_proxy")
	_ = os.Unsetenv("no_proxy")

	env := hostBaseEnv()

	for _, key := range []string{
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"NO_PROXY=",
		"http_proxy=",
		"https_proxy=",
		"no_proxy=",
	} {
		for _, entry := range env {
			if strings.HasPrefix(entry, key) {
				t.Fatalf("did not expect %s in env=%v", key, env)
			}
		}
	}
}
