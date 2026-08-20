package worker

import (
	"strings"
	"testing"
)

// Nothing derives the WARP_ names any more: each set site writes both outright. This is the
// guard that keeps them in step, by failing when a well-known variable carries an OZ_ name
// without a WARP_ one holding the same value.
//
// It covers the pair helpers rather than each backend's full environment, because those also
// carry the host environment and operator config, neither of which is paired on purpose.
func TestBackendEnvPairsEveryOZName(t *testing.T) {
	groups := map[string][]string{
		"run ID":           runIDEnvVars("task-1"),
		"execution ID":     executionIDEnvVars("exec-1"),
		"worker backend":   workerBackendEnvVars(directBackendTypeName),
		"workspace root":   workspaceRootEnvVars("/workspace"),
		"environment file": environmentFileEnvVars("/tmp/oz-env"),
		"server root URL":  serverRootURLEnvVars("https://app.warp.dev/?a=b"),
		"docker image":     dockerImageEnvVars("ubuntu:22.04"),
	}

	for name, group := range groups {
		t.Run(name, func(t *testing.T) {
			env := envMap(group)
			paired := 0
			for envName, value := range env {
				suffix, isOZ := strings.CutPrefix(envName, "OZ_")
				if !isOZ {
					continue
				}
				paired++
				if alias := "WARP_" + suffix; env[alias] != value {
					t.Errorf("%s = %q, want a matching %s with the same value, got %q",
						envName, value, alias, env[alias])
				}
			}
			if paired == 0 {
				t.Errorf("no OZ_ variables in this group, so the assertion above proves nothing")
			}
			// Every entry is either an OZ_ name or its WARP_ counterpart, so a group that
			// grew a third, unpaired entry fails here.
			if len(env) != 2*paired {
				t.Errorf("group has %d entries for %d OZ_ names; expected exactly one WARP_ name each", len(env), paired)
			}
		})
	}
}

// A value containing '=' reaches both names intact.
func TestBackendEnvPairPreservesValuesContainingEquals(t *testing.T) {
	env := envMap(serverRootURLEnvVars("https://app.warp.dev/?a=b"))
	const want = "https://app.warp.dev/?a=b"
	if env["OZ_SERVER_ROOT_URL"] != want {
		t.Errorf("OZ_SERVER_ROOT_URL = %q, want %q", env["OZ_SERVER_ROOT_URL"], want)
	}
	if env["WARP_SERVER_ROOT_URL"] != want {
		t.Errorf("WARP_SERVER_ROOT_URL = %q, want %q", env["WARP_SERVER_ROOT_URL"], want)
	}
}

func TestConcatEnvVars(t *testing.T) {
	got := concatEnvVars(
		runIDEnvVars("task-1"),
		nil,
		workerBackendEnvVars(directBackendTypeName),
	)
	want := []string{
		"OZ_RUN_ID=task-1",
		"WARP_RUN_ID=task-1",
		"OZ_WORKER_BACKEND=direct",
		"WARP_WORKER_BACKEND=direct",
	}
	if len(got) != len(want) {
		t.Fatalf("concatEnvVars() = %v, want %v", got, want)
	}
	for i, entry := range want {
		if got[i] != entry {
			t.Errorf("entry %d = %q, want %q", i, got[i], entry)
		}
	}
}
