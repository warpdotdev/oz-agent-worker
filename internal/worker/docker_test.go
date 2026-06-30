package worker

import (
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func TestDockerResourcesForShape(t *testing.T) {
	const giB = int64(1) << 30
	tests := []struct {
		name         string
		shape        *types.InstanceShape
		wantNanoCPUs int64
		wantMemory   int64
	}{
		{name: "nil shape"},
		{name: "zero axes", shape: &types.InstanceShape{}},
		{name: "negative axes", shape: &types.InstanceShape{Vcpus: -1, MemoryGb: -2}},
		{name: "full shape", shape: &types.InstanceShape{Vcpus: 4, MemoryGb: 16}, wantNanoCPUs: 4_000_000_000, wantMemory: 16 * giB},
		{name: "cpu only", shape: &types.InstanceShape{Vcpus: 2}, wantNanoCPUs: 2_000_000_000},
		{name: "memory only", shape: &types.InstanceShape{MemoryGb: 8}, wantMemory: 8 * giB},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := dockerResourcesForShape(tc.shape)
			if res.NanoCPUs != tc.wantNanoCPUs {
				t.Errorf("NanoCPUs = %d, want %d", res.NanoCPUs, tc.wantNanoCPUs)
			}
			if res.Memory != tc.wantMemory {
				t.Errorf("Memory = %d, want %d", res.Memory, tc.wantMemory)
			}
		})
	}
}
