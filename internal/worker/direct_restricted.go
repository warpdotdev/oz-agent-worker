package worker

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const restrictedGitModeEnv = "WARP_GITHUB_CREDENTIAL_MODE=restricted_installation"

type directEnvSource int

const (
	assignmentEnv directEnvSource = iota
	workerEnv
	setupEnv
)

// restrictedGitEnv keeps task credentials from the assignment, never from an
// operator's environment or a hook. The worker reports names, not values.
func restrictedGitEnv(entries []string, source directEnvSource) ([]string, error) {
	if source == assignmentEnv && !slices.Contains(entries, restrictedGitModeEnv) {
		return nil, errors.New("private Git home requires the restricted assignment credential mode")
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "WARP_GITHUB_CREDENTIAL_MODE":
			if entry != restrictedGitModeEnv {
				return nil, errors.New("conflicting restricted Git credential mode")
			}
		case "WARP_API_KEY", "WARP_WORKLOAD_TOKEN", "WARP_EXECUTION_ID", "OZ_EXECUTION_ID", "TASK_ID", "WARP_RUN_ID", "OZ_RUN_ID", "WARP_SERVER_ROOT_URL":
			if source != assignmentEnv {
				return nil, fmt.Errorf("private Git home does not allow an override of %s", key)
			}
		case "GIT_TERMINAL_PROMPT", "GH_PROMPT_DISABLED":
			if entry != "GIT_TERMINAL_PROMPT=0" && entry != "GH_PROMPT_DISABLED=1" {
				return nil, fmt.Errorf("private Git home does not allow an override of %s", key)
			}
		case "GH_CONFIG_DIR":
			// Local worker launchers can configure gh for unrelated tasks.
			if source == workerEnv {
				continue
			}
			return nil, errors.New("private Git home does not allow an override of GH_CONFIG_DIR")
		default:
			if strings.HasPrefix(key, "GH_") || strings.HasPrefix(key, "GITHUB_") ||
				strings.HasPrefix(key, "SSH_") || strings.HasPrefix(key, "GCM_") || key == "OZ_LOCAL_GH_TOKEN" {
				continue
			}
			if key == "HOME" || key == "NETRC" || key == "CURL_HOME" ||
				strings.HasPrefix(key, "XDG_") || strings.HasPrefix(key, "GIT_") {
				return nil, fmt.Errorf("private Git home does not allow an override of %s", key)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}
