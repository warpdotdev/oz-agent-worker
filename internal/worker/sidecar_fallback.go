package worker

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
)

// A Claude Code or Codex run driven over the Agent Client Protocol is assigned the
// harness's ACP sidecar image, at the same mount path its CLI sidecar uses. When the
// ACP image cannot be pulled (Docker Hub blocked, a mirror that lacks it), the Docker
// and Kubernetes backends mount the CLI sidecar instead and let the task continue: the
// agent finds no ACP adapter under the mount and drives the harness through a terminal
// by itself, reporting why to warp-server.
const (
	officialSidecarRepositoryPrefix = "docker.io/warpdotdev/warp-"
	officialACPSidecarSuffix        = "-acp-sidecar"
	officialCLISidecarSuffix        = "-cli-sidecar"
	// fallbackCLISidecarTag is the tag of the CLI sidecar used as a fallback. The server
	// pins the ACP image by digest, so there is no tag to carry over; latest is the tag
	// the server's own terminal configuration names for the CLI sidecars.
	fallbackCLISidecarTag = "latest"

	// sidecarPullFailureTTL is how long a backend keeps mounting the fallback for an
	// ACP image that failed to pull, without trying that image again.
	sidecarPullFailureTTL = 10 * time.Minute

	sidecarFallbackTriggerPullFailed    = "image_pull_failed"
	sidecarFallbackTriggerRecentFailure = "recent_pull_failure"
)

// officialCLISidecarForACP returns the CLI sidecar that stands in for assigned when
// assigned names an official ACP sidecar repository, ignoring its tag or digest.
func officialCLISidecarForACP(assigned string) (string, bool) {
	named, err := reference.ParseNormalizedNamed(assigned)
	if err != nil {
		return "", false
	}
	rest, ok := strings.CutPrefix(reference.TrimNamed(named).String(), officialSidecarRepositoryPrefix)
	if !ok {
		return "", false
	}
	harness, ok := strings.CutSuffix(rest, officialACPSidecarSuffix)
	if !ok || harness == "" || strings.Contains(harness, "/") {
		return "", false
	}
	return officialSidecarRepositoryPrefix + harness + officialCLISidecarSuffix + ":" + fallbackCLISidecarTag, true
}

// sidecarFallbackImage returns the fallback for a sidecar assigned as assigned and
// mounted as remapped, or "" when it has none. The fallback goes through the same
// mappings as the assigned image. It is dropped when it resolves to the image already
// mounted, as with a legacy coding_cli_sidecars entry pinned to one image for both
// repositories, since retrying the same pull cannot help.
func sidecarFallbackImage(remapper *common.ImageRemapper, assigned, remapped string) string {
	cli, ok := officialCLISidecarForACP(assigned)
	if !ok {
		return ""
	}
	fallback := remapper.Remap(cli)
	if sameImageReference(fallback, remapped) {
		return ""
	}
	return fallback
}

func sameImageReference(a, b string) bool {
	return canonicalImageReference(a) == canonicalImageReference(b)
}

// canonicalImageReference normalizes an image reference the way container runtimes
// do (docker.io domain, library/ namespace, latest tag), or returns it unchanged when
// it does not parse.
func canonicalImageReference(image string) string {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image
	}
	return reference.TagNameOnly(named).String()
}

// withSidecarFallbacks returns sidecars with every entry for which swap reports true
// replaced by its fallback, and whether any was. Only entries with a fallback are
// offered to swap, and a replaced entry keeps no fallback, so it is never swapped
// twice. The input is never modified; it is returned as is when nothing was swapped.
func withSidecarFallbacks(sidecars []types.SidecarMount, swap func(types.SidecarMount) bool) ([]types.SidecarMount, bool) {
	var swapped []types.SidecarMount
	for i, sidecar := range sidecars {
		if sidecar.FallbackImage == "" || !swap(sidecar) {
			continue
		}
		if swapped == nil {
			swapped = slices.Clone(sidecars)
		}
		swapped[i].Image = sidecar.FallbackImage
		swapped[i].FallbackImage = ""
	}
	if swapped == nil {
		return sidecars, false
	}
	return swapped, true
}

// reportSidecarFallback logs that sidecar's fallback is mounted in place of its image
// and records it on the task span. cause is the pull failure, or nil when the image
// failed recently and was not tried again. Image references stay out of span
// attributes, since they can name operator registries.
func reportSidecarFallback(ctx context.Context, backend string, sidecar types.SidecarMount, cause error) {
	trigger := sidecarFallbackTriggerRecentFailure
	if cause != nil {
		trigger = sidecarFallbackTriggerPullFailed
		log.Warnf(ctx, "ACP sidecar image %s could not be pulled; mounting %s at %s instead, so the agent will drive the harness through a terminal: %v",
			sidecar.Image, sidecar.FallbackImage, sidecar.MountPath, cause)
	} else {
		log.Warnf(ctx, "ACP sidecar image %s failed to pull within the last %s; mounting %s at %s instead, so the agent will drive the harness through a terminal",
			sidecar.Image, sidecarPullFailureTTL, sidecar.FallbackImage, sidecar.MountPath)
	}
	metrics.AddTaskEvent(ctx, "sidecar.fallback",
		attribute.String("backend", backend),
		attribute.String("trigger", trigger),
		attribute.String("mount_path", sidecar.MountPath))
}

// sidecarPullFailureCache remembers sidecar images that recently failed to pull, so a
// backend in an environment that cannot reach them mounts the fallback straight away
// instead of paying for a failed pull on every task. Keys are the exact references a
// backend tried, so a new digest or a changed mapping is tried again at once. The zero
// value is ready to use.
type sidecarPullFailureCache struct {
	mu       sync.Mutex
	failedAt map[string]time.Time
}

func (c *sidecarPullFailureCache) record(image string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failedAt == nil {
		c.failedAt = make(map[string]time.Time)
	}
	c.failedAt[image] = time.Now()
}

// recent reports whether image failed to pull within sidecarPullFailureTTL.
func (c *sidecarPullFailureCache) recent(image string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for failed, at := range c.failedAt {
		if now.Sub(at) >= sidecarPullFailureTTL {
			delete(c.failedAt, failed)
		}
	}
	_, ok := c.failedAt[image]
	return ok
}
