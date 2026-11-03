package worker

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
)

// acpHarnessCapabilityPrefix prefixes the per-harness capability tokens the worker sends in
// X-Warp-Worker-Capabilities, one "acp-harness-v1:<harness>" token for each harness it can drive
// over ACP. It mirrors selfhosted.ACPHarnessCapabilityPrefix in warp-server
// (logic/ai/ambient_agents/workers/selfhosted/websocket.go). The server drives a harness over a
// PTY on any connection that did not advertise it, so a worker that sends no tokens keeps the
// pre-ACP behavior.
const acpHarnessCapabilityPrefix = "acp-harness-v1:"

// acpHarness is a third-party harness warp-server may drive over the Agent Client Protocol
// (ACP) on self-hosted workers instead of a PTY.
type acpHarness struct {
	// name is the harness config name warp-server uses, e.g. in oz.acp_harnesses.
	name string
	// sidecarRepository is the official ACP sidecar repository the server assigns for the
	// harness on container backends.
	sidecarRepository string
	// adapterBinary is the ACP adapter the Warp agent resolves on PATH when no sidecar
	// manifest is mounted, which is the case on the direct backend.
	adapterBinary string
}

// knownACPHarnesses lists the harnesses the server can switch to ACP. The adapter binaries
// match the Warp agent's built-in ACP defaults.
var knownACPHarnesses = []acpHarness{
	{name: "claude", sidecarRepository: "warpdotdev/warp-claude-acp-sidecar", adapterBinary: "claude-code-acp"},
	{name: "codex", sidecarRepository: "warpdotdev/warp-codex-acp-sidecar", adapterBinary: "codex-acp"},
}

func knownACPHarnessNames() []string {
	names := make([]string, 0, len(knownACPHarnesses))
	for _, h := range knownACPHarnesses {
		names = append(names, h.name)
	}
	return names
}

// resolveACPHarnesses returns the sorted names of the harnesses this worker advertises ACP
// support for. An operator override (config.ACPHarnesses) wins; otherwise the list is
// detected from the backend:
//   - docker and kubernetes can materialize the official ACP sidecar image for every known
//     harness, except one whose legacy coding_cli_sidecars alias remaps that image to a CLI
//     image (config.LegacyCodingCLISidecarHarnesses).
//   - direct runs the Warp agent on the host, so a harness is advertised only when its ACP
//     adapter resolves on the PATH tasks will see.
//   - command hands sidecars to an operator-owned runtime the worker cannot inspect, so nothing
//     is advertised without an override.
//
// The result is computed once at startup; changing it needs a worker restart.
func resolveACPHarnesses(ctx context.Context, config Config) ([]string, error) {
	if config.ACPHarnesses != nil {
		names, err := validateACPHarnessOverride(*config.ACPHarnesses)
		if err != nil {
			return nil, err
		}
		logAdvertisedACPHarnesses(ctx, names, "configured")
		return names, nil
	}

	var names []string
	switch config.BackendType {
	case "docker", "", "kubernetes":
		for _, h := range knownACPHarnesses {
			if slices.Contains(config.LegacyCodingCLISidecarHarnesses, h.name) {
				log.Warnf(ctx, "backend.%s.coding_cli_sidecars.%s remaps %s to a CLI image, so %s runs over a PTY on this worker; "+
					"mirror the ACP repository with sidecar_image_mappings or set acp_harnesses to advertise ACP",
					backendTypeName(config.BackendType), h.name, h.sidecarRepository, h.name)
				continue
			}
			names = append(names, h.name)
		}
	case "direct":
		var pathEnv string
		if config.Direct != nil {
			pathEnv = config.Direct.Env["PATH"]
		}
		for _, h := range knownACPHarnesses {
			if _, err := lookPathIn(pathEnv, h.adapterBinary); err != nil {
				log.Infof(ctx, "%s not found on PATH; %s runs over a PTY on this worker", h.adapterBinary, h.name)
				continue
			}
			names = append(names, h.name)
		}
	case "command":
		// The dispatch runtime may or may not mount the payload's sidecars.
	}
	slices.Sort(names)
	logAdvertisedACPHarnesses(ctx, names, "auto-detected")
	return names, nil
}

func backendTypeName(backendType string) string {
	if backendType == "" {
		return "docker"
	}
	return backendType
}

func validateACPHarnessOverride(override []string) ([]string, error) {
	known := knownACPHarnessNames()
	names := make([]string, 0, len(override))
	for _, name := range override {
		name = strings.TrimSpace(name)
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("invalid acp_harnesses entry %q: valid harnesses are %s", name, strings.Join(known, ", "))
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func logAdvertisedACPHarnesses(ctx context.Context, names []string, source string) {
	if len(names) == 0 {
		log.Infof(ctx, "Advertising ACP support for harnesses: none (%s)", source)
		return
	}
	log.Infof(ctx, "Advertising ACP support for harnesses: %v (%s)", names, source)
}

// lookPathIn resolves file against the directories of pathEnv, the PATH value the direct
// backend hands to tasks when backend.direct.environment overrides it. An empty pathEnv uses
// the worker's own PATH through exec.LookPath, which is what tasks inherit by default.
func lookPathIn(pathEnv, file string) (string, error) {
	if pathEnv == "" {
		return exec.LookPath(file)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH %q", file, pathEnv)
}

// acpHarnessCapabilities turns advertised harness names into capability header tokens.
func acpHarnessCapabilities(names []string) []string {
	tokens := make([]string, 0, len(names))
	for _, name := range names {
		tokens = append(tokens, acpHarnessCapabilityPrefix+name)
	}
	return tokens
}

// capabilityHeader is the X-Warp-Worker-Capabilities value sent on every connect: the fixed
// capabilities first, then one token per harness the worker drives over ACP.
func (w *Worker) capabilityHeader() string {
	capabilities := append([]string{executionCancellationCapability}, acpHarnessCapabilities(w.acpHarnesses)...)
	return strings.Join(capabilities, ",")
}
