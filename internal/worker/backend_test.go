package worker

import (
	"slices"
	"testing"
)

func TestWithWarpAliases(t *testing.T) {
	tests := []struct {
		name    string
		envVars []string
		want    []string
	}{
		{
			name:    "OZ_ entries gain a WARP_ alias with the same value",
			envVars: []string{"OZ_RUN_ID=task-1", "OZ_WORKER_BACKEND=direct"},
			want: []string{
				"OZ_RUN_ID=task-1",
				"OZ_WORKER_BACKEND=direct",
				"WARP_RUN_ID=task-1",
				"WARP_WORKER_BACKEND=direct",
			},
		},
		{
			name:    "entries that are not OZ_-prefixed are left alone",
			envVars: []string{"GIT_CONFIG_GLOBAL=/tmp/.gitconfig", "WARP_API_KEY=key", "OZONE=layer"},
			want:    []string{"GIT_CONFIG_GLOBAL=/tmp/.gitconfig", "WARP_API_KEY=key", "OZONE=layer"},
		},
		{
			name:    "a value containing = is preserved in the alias",
			envVars: []string{"OZ_SERVER_ROOT_URL=https://app.warp.dev/?a=b"},
			want: []string{
				"OZ_SERVER_ROOT_URL=https://app.warp.dev/?a=b",
				"WARP_SERVER_ROOT_URL=https://app.warp.dev/?a=b",
			},
		},
		{
			name:    "an entry with no value separator has no name to alias",
			envVars: []string{"OZ_RUN_ID"},
			want:    []string{"OZ_RUN_ID"},
		},
		{
			name:    "an empty environment stays empty",
			envVars: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// slices.Equal rather than reflect.DeepEqual: a nil and an empty result are
			// indistinguishable to every caller, so pinning which one comes back would
			// only break on a harmless refactor.
			if got := withWarpAliases(tt.envVars); !slices.Equal(got, tt.want) {
				t.Fatalf("withWarpAliases(%v) = %v, want %v", tt.envVars, got, tt.want)
			}
		})
	}
}
