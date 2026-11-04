package worker

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func TestSidecarMountsComputesACPFallback(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	claudeACP := "index.docker.io/warpdotdev/warp-claude-acp-sidecar@" + digest
	const officialClaudeCLI = "docker.io/warpdotdev/warp-claude-cli-sidecar:latest"

	for _, tc := range []struct {
		name         string
		mappings     map[string]string
		assigned     string
		wantImage    string
		wantFallback string
	}{
		{
			name:         "unmapped ACP image pinned by digest falls back to the official CLI image",
			assigned:     claudeACP,
			wantImage:    claudeACP,
			wantFallback: officialClaudeCLI,
		},
		{
			name:         "the harness and any tag come from the ACP repository, not the CLI tag",
			assigned:     "warpdotdev/warp-codex-acp-sidecar:v1",
			wantImage:    "warpdotdev/warp-codex-acp-sidecar:v1",
			wantFallback: "docker.io/warpdotdev/warp-codex-cli-sidecar:latest",
		},
		{
			name:         "a mapping for only the CLI repository applies to the fallback",
			mappings:     map[string]string{"warpdotdev/warp-claude-cli-sidecar": "mirror.example/claude-cli"},
			assigned:     claudeACP,
			wantImage:    claudeACP,
			wantFallback: "mirror.example/claude-cli:latest",
		},
		{
			name: "both repositories mapped",
			mappings: map[string]string{
				"warpdotdev/warp-claude-acp-sidecar": "mirror.example/claude-acp",
				"warpdotdev/warp-claude-cli-sidecar": "mirror.example/claude-cli:pinned",
			},
			assigned:     claudeACP,
			wantImage:    "mirror.example/claude-acp@" + digest,
			wantFallback: "mirror.example/claude-cli:pinned",
		},
		{
			name: "a legacy entry pinned for both repositories has no fallback",
			mappings: map[string]string{
				"warpdotdev/warp-claude-acp-sidecar": "registry.example/wrapper:v1",
				"warpdotdev/warp-claude-cli-sidecar": "registry.example/wrapper:v1",
			},
			assigned:  claudeACP,
			wantImage: "registry.example/wrapper:v1",
		},
		{
			name: "a legacy entry left unpinned falls back from the carried-over ACP digest to latest",
			mappings: map[string]string{
				"warpdotdev/warp-claude-acp-sidecar": "registry.example/wrapper",
				"warpdotdev/warp-claude-cli-sidecar": "registry.example/wrapper",
			},
			assigned:     claudeACP,
			wantImage:    "registry.example/wrapper@" + digest,
			wantFallback: "registry.example/wrapper:latest",
		},
		{name: "CLI sidecar", assigned: "warpdotdev/warp-claude-cli-sidecar:v1", wantImage: "warpdotdev/warp-claude-cli-sidecar:v1"},
		{name: "xvfb sidecar", assigned: "warpdotdev/warp-xvfb-sidecar:v1", wantImage: "warpdotdev/warp-xvfb-sidecar:v1"},
		{name: "no harness", assigned: "warpdotdev/warp-acp-sidecar:v1", wantImage: "warpdotdev/warp-acp-sidecar:v1"},
		{name: "unofficial registry", assigned: "other.example/warpdotdev/warp-claude-acp-sidecar:v1", wantImage: "other.example/warpdotdev/warp-claude-acp-sidecar:v1"},
		{name: "unofficial namespace", assigned: "someone/warp-claude-acp-sidecar:v1", wantImage: "someone/warp-claude-acp-sidecar:v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var remapper *common.ImageRemapper
			if tc.mappings != nil {
				var err error
				if remapper, err = common.NewImageRemapper(tc.mappings); err != nil {
					t.Fatal(err)
				}
			}
			w := &Worker{ctx: context.Background(), config: Config{SidecarImageMappings: remapper}}
			sidecars := w.sidecarMounts("docker.io/warpdotdev/warp-agent:v1", []types.SidecarMount{{Image: tc.assigned, MountPath: "/mnt/harness"}})
			want := []types.SidecarMount{
				{Image: "docker.io/warpdotdev/warp-agent:v1", MountPath: "/agent"},
				{Image: tc.wantImage, MountPath: "/mnt/harness", FallbackImage: tc.wantFallback},
			}
			if !reflect.DeepEqual(sidecars, want) {
				t.Fatalf("sidecars = %+v, want %+v", sidecars, want)
			}
		})
	}
}

func TestTaskAndStandbyParamsCarryACPFallback(t *testing.T) {
	acp := types.SidecarMount{Image: "warpdotdev/warp-claude-acp-sidecar:v1", MountPath: "/mnt/claude-cli-sidecar"}
	want := []types.SidecarMount{{Image: acp.Image, MountPath: acp.MountPath, FallbackImage: "docker.io/warpdotdev/warp-claude-cli-sidecar:latest"}}
	w := &Worker{ctx: context.Background()}

	assignment := &types.TaskAssignmentMessage{TaskID: "task-1", Task: &types.Task{ID: "task-1"}, AdditionalSidecars: []types.SidecarMount{acp}}
	if got := w.prepareTaskParams(assignment).Sidecars; !reflect.DeepEqual(got, want) {
		t.Fatalf("task sidecars = %+v, want %+v", got, want)
	}
	if assignment.AdditionalSidecars[0] != acp {
		t.Fatalf("assignment was mutated: %+v", assignment.AdditionalSidecars[0])
	}

	create := &types.PrewarmCreateMessage{SandboxID: "standby-1", AdditionalSidecars: []types.SidecarMount{acp}}
	if got := w.prepareStandbyTaskParams(create).Sidecars; !reflect.DeepEqual(got, want) {
		t.Fatalf("standby sidecars = %+v, want %+v", got, want)
	}
}

func TestWithSidecarFallbacks(t *testing.T) {
	sidecars := []types.SidecarMount{
		{Image: "agent", MountPath: "/agent"},
		{Image: "acp-a", MountPath: "/mnt/a", FallbackImage: "cli-a"},
		{Image: "acp-b", MountPath: "/mnt/b", FallbackImage: "cli-b", ReadWrite: true},
	}
	original := append([]types.SidecarMount(nil), sidecars...)

	got, swapped := withSidecarFallbacks(sidecars, func(s types.SidecarMount) bool { return s.Image == "acp-b" })
	want := []types.SidecarMount{original[0], original[1], {Image: "cli-b", MountPath: "/mnt/b", ReadWrite: true}}
	if !swapped || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v (swapped=%v), want %+v", got, swapped, want)
	}
	if !reflect.DeepEqual(sidecars, original) {
		t.Fatalf("input was modified: %+v", sidecars)
	}

	var offered []string
	got, swapped = withSidecarFallbacks(sidecars, func(s types.SidecarMount) bool {
		offered = append(offered, s.Image)
		return false
	})
	if swapped || !reflect.DeepEqual(got, sidecars) || !reflect.DeepEqual(offered, []string{"acp-a", "acp-b"}) {
		t.Fatalf("got %+v (swapped=%v, offered=%v); only sidecars with a fallback may be offered", got, swapped, offered)
	}
}

func TestSidecarPullFailureCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache sidecarPullFailureCache
		if cache.recent("acp") {
			t.Fatal("zero cache reported a failure")
		}
		cache.record("acp")
		time.Sleep(sidecarPullFailureTTL - time.Second)
		if !cache.recent("acp") || cache.recent("acp@other-digest") {
			t.Fatal("failure must be remembered for its exact reference until the TTL")
		}
		time.Sleep(time.Second)
		if cache.recent("acp") {
			t.Fatal("failure must expire after the TTL")
		}
		if len(cache.failedAt) != 0 {
			t.Fatalf("expired entries were not pruned: %v", cache.failedAt)
		}
	})
}
