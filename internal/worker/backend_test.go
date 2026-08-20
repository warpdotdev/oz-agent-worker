package worker

import (
	"strings"
	"testing"
)

// Nothing derives the WARP_ names any more: each set site writes both outright. This is the
// guard that keeps them in step.
//
// It runs over the environment each backend actually builds, not over the pair helpers, so it
// also catches a variable added inline at a call site or a new helper nobody registered here.
// Each function returns only the worker-owned slice, which excludes the host environment and
// operator config by construction — neither of those is paired on purpose.
func TestBackendEnvPairsEveryOZName(t *testing.T) {
	backendEnvs := map[string][]string{
		"command dispatch": commandDispatchEnvVars("task-1", "exec-1", "https://app.warp.dev/?a=b", "ubuntu:22.04"),
		"command cancel":   commandCancelEnvVars("task-1", "exec-1"),
		"direct setup":     directSetupEnvVars("/workspace", "task-1", "/tmp/oz-env"),
		"direct teardown":  directTeardownEnvVars("/workspace", "/workspace/.gitconfig", "task-1"),
		"kubernetes task":  kubernetesTaskOwnedEnvVars("task-1"),
	}

	for name, envVars := range backendEnvs {
		t.Run(name, func(t *testing.T) {
			env := envMap(envVars)
			ozNames, warpNames := 0, 0
			for envName, value := range env {
				if strings.HasPrefix(envName, "WARP_") {
					warpNames++
					continue
				}
				suffix, isOZ := strings.CutPrefix(envName, "OZ_")
				if !isOZ {
					// Something like GIT_CONFIG_GLOBAL, which has no OZ_/WARP_ spelling.
					continue
				}
				ozNames++
				if warpName := "WARP_" + suffix; env[warpName] != value {
					t.Errorf("%s = %q, want a matching %s with the same value, got %q",
						envName, value, warpName, env[warpName])
				}
			}
			if ozNames == 0 {
				t.Errorf("no OZ_ variables in this environment, so the assertion above proves nothing")
			}
			// Counting both directions catches a WARP_ name left behind after its OZ_
			// counterpart was renamed or removed, which the per-name check above cannot see.
			if ozNames != warpNames {
				t.Errorf("%d OZ_ names but %d WARP_ names; every one should be paired", ozNames, warpNames)
			}
		})
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
