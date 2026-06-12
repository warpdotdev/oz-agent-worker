package worker

import "testing"

func TestSanitizeVolumeName(t *testing.T) {
	tests := []struct {
		name      string
		imageName string
		digest    string
		want      string
	}{
		{
			name:      "docker hub image",
			imageName: "warpdotdev/warp-agent:latest-dev",
			digest:    "sha256:cdd2970ebf33dea820d96c39246d0db275f1e33965e9b65381e778eb77a7a1af",
			want:      "warpdotdev-warp-agent-cdd2970ebf33",
		},
		{
			name:      "registry host with port",
			imageName: "localhost:5000/warp-agent:pr120",
			digest:    "sha256:cdd2970ebf33dea820d96c39246d0db275f1e33965e9b65381e778eb77a7a1af",
			want:      "localhost-5000-warp-agent-cdd2970ebf33",
		},
		{
			name:      "registry host with port and nested repo",
			imageName: "registry.example.com:8443/team/agent:v1",
			digest:    "sha256:abcdef0123456789",
			want:      "registry.example.com-8443-team-agent-abcdef012345",
		},
		{
			name:      "malformed digest falls back to base name plus digest",
			imageName: "warpdotdev/warp-agent",
			digest:    "not-a-digest",
			want:      "warpdotdev-warp-agent-not-a-digest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeVolumeName(tt.imageName, tt.digest)
			if got != tt.want {
				t.Errorf("sanitizeVolumeName(%q, %q) = %q, want %q", tt.imageName, tt.digest, got, tt.want)
			}
		})
	}
}
