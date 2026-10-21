package common

import (
	"strings"
	"testing"
)

func TestImageRemapper(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	otherDigest := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, source, destination, image, want string
	}{
		{"digest preserved", "warpdotdev/warp-agent", "mirror.example/agent", "warpdotdev/warp-agent@" + digest, "mirror.example/agent@" + digest},
		{"tag and digest preserved", "warpdotdev/warp-agent", "mirror.example/agent", "warpdotdev/warp-agent:v123@" + digest, "mirror.example/agent:v123@" + digest},
		{"tag pin replaces source digest", "warpdotdev/warp-agent", "mirror.example/agent:approved", "warpdotdev/warp-agent:v123@" + digest, "mirror.example/agent:approved"},
		{"tag and digest pin", "warpdotdev/warp-agent", "mirror.example/agent:approved@" + otherDigest, "warpdotdev/warp-agent@" + digest, "mirror.example/agent:approved@" + otherDigest},
		{"implicit latest", "warpdotdev/warp-agent", "mirror.example/agent", "warpdotdev/warp-agent", "mirror.example/agent"},
		{"registry port", "source.example:5000/agent", "mirror.example:5001/agent", "source.example:5000/agent:v1", "mirror.example:5001/agent:v1"},
		{"default library repository", "busybox", "mirror.example/busybox", "docker.io/library/busybox:1.36", "mirror.example/busybox:1.36"},
		{"exact repository match", "warpdotdev/warp-agent", "mirror.example/agent", "warpdotdev/warp-agent-extra:v123", "warpdotdev/warp-agent-extra:v123"},
		{"invalid assignment left for backend validation", "warpdotdev/warp-agent", "mirror.example/agent", "invalid image", "invalid image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remapper, err := NewImageRemapper(map[string]string{tc.source: tc.destination})
			if err != nil {
				t.Fatal(err)
			}
			if got := remapper.Remap(tc.image); got != tc.want {
				t.Fatalf("Remap(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}

func TestImageRemapperRejectsInvalidMappings(t *testing.T) {
	for _, mappings := range []map[string]string{
		{"": "mirror.example/agent"},
		{"bad source": "mirror.example/agent"},
		{"warpdotdev/warp-agent:latest": "mirror.example/agent"},
		{"warpdotdev/warp-agent@sha256:" + strings.Repeat("a", 64): "mirror.example/agent"},
		{"warpdotdev/warp-agent": ""},
		{"warpdotdev/warp-agent": "https://mirror.example/agent"},
		{"warpdotdev/warp-agent": "mirror.example/agent@sha256:bad"},
		{"warpdotdev/warp-agent": "mirror.example/agent", "docker.io/warpdotdev/warp-agent": "other.example/agent"},
	} {
		if _, err := NewImageRemapper(mappings); err == nil {
			t.Fatalf("expected invalid mapping error for %v", mappings)
		}
	}
}

func TestImageRemapperDoesNotChainMappings(t *testing.T) {
	mappings := map[string]string{"source.example/agent": "mirror.example/agent", "mirror.example/agent": "other.example/agent"}
	remapper, err := NewImageRemapper(mappings)
	if err != nil {
		t.Fatal(err)
	}
	mappings["source.example/agent"] = "mutated.example/agent"
	if got := remapper.Remap("source.example/agent:v1"); got != "mirror.example/agent:v1" {
		t.Fatalf("Remap = %q, want single-pass immutable mapping", got)
	}
}
