package worker

import (
	"fmt"
	"strings"
)

const metadataEnvPrefix = "WARP_METADATA_"

func validateMetadataEnvConflicts(taskEnv, configuredEnv []string) error {
	names := make(map[string]struct{})
	for _, entry := range taskEnv {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, metadataEnvPrefix) {
			names[name] = struct{}{}
		}
	}
	for _, entry := range configuredEnv {
		name, _, _ := strings.Cut(entry, "=")
		if _, exists := names[strings.ToUpper(name)]; exists {
			return fmt.Errorf("metadata environment variable %s conflicts with configured environment variable %q", strings.ToUpper(name), name)
		}
	}
	return nil
}
