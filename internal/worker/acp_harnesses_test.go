package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func acpHarnessList(names ...string) *[]string {
	list := append([]string{}, names...)
	return &list
}

func TestResolveACPHarnesses(t *testing.T) {
	both := []string{"claude", "codex"}
	tests := []struct {
		name    string
		config  Config
		want    []string
		wantErr string
	}{
		{name: "docker advertises every known harness", config: Config{BackendType: "docker"}, want: both},
		{name: "the default backend is docker", config: Config{}, want: both},
		{name: "kubernetes advertises every known harness", config: Config{BackendType: "kubernetes"}, want: both},
		{
			name:   "a legacy coding_cli_sidecars alias drops its harness",
			config: Config{BackendType: "kubernetes", LegacyCodingCLISidecarHarnesses: []string{"claude"}},
			want:   []string{"codex"},
		},
		{
			name:   "aliases for every harness advertise none",
			config: Config{BackendType: "kubernetes", LegacyCodingCLISidecarHarnesses: both},
			want:   nil,
		},
		{name: "an empty override advertises none", config: Config{BackendType: "docker", ACPHarnesses: acpHarnessList()}, want: nil},
		{
			name:   "an override wins over the alias rule",
			config: Config{BackendType: "kubernetes", LegacyCodingCLISidecarHarnesses: both, ACPHarnesses: acpHarnessList("codex")},
			want:   []string{"codex"},
		},
		{
			name:   "an override is deduplicated and sorted",
			config: Config{BackendType: "docker", ACPHarnesses: acpHarnessList("codex", " claude ", "codex")},
			want:   both,
		},
		{
			name:    "an unknown override entry fails startup",
			config:  Config{BackendType: "docker", ACPHarnesses: acpHarnessList("claud")},
			wantErr: `invalid acp_harnesses entry "claud": valid harnesses are claude, codex`,
		},
		{name: "command advertises none by default", config: Config{BackendType: "command"}, want: nil},
		{name: "command honours an override", config: Config{BackendType: "command", ACPHarnesses: acpHarnessList("claude")}, want: []string{"claude"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveACPHarnesses(context.Background(), tc.config)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("harnesses = %v, want %v", got, tc.want)
			}
		})
	}
}

// writeStubBinary creates an executable file named name in dir.
func writeStubBinary(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveACPHarnessesDirectProbesAdapters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec.LookPath needs PATHEXT-aware stubs on Windows")
	}
	tests := []struct {
		name     string
		hostBins []string
		envBins  []string // nil leaves backend.direct.environment PATH unset
		want     []string
	}{
		{name: "both adapters on the host PATH", hostBins: []string{"claude-code-acp", "codex-acp"}, want: []string{"claude", "codex"}},
		{name: "only the codex adapter", hostBins: []string{"codex-acp"}, want: []string{"codex"}},
		{name: "no adapters", hostBins: nil, want: nil},
		{
			name:     "a PATH from backend.direct.environment replaces the host PATH",
			hostBins: []string{"codex-acp"},
			envBins:  []string{"claude-code-acp"},
			want:     []string{"claude"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hostDir := t.TempDir()
			for _, name := range tc.hostBins {
				writeStubBinary(t, hostDir, name)
			}
			t.Setenv("PATH", hostDir)
			config := Config{BackendType: "direct", Direct: &DirectBackendConfig{Env: map[string]string{}}}
			if tc.envBins != nil {
				envDir := t.TempDir()
				for _, name := range tc.envBins {
					writeStubBinary(t, envDir, name)
				}
				config.Direct.Env["PATH"] = envDir
			}
			got, err := resolveACPHarnesses(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("harnesses = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCapabilityHeader(t *testing.T) {
	if got := (&Worker{}).capabilityHeader(); got != "execution-scoped-cancellation-v1" {
		t.Fatalf("header without ACP harnesses = %q", got)
	}
	w := &Worker{acpHarnesses: []string{"claude", "codex"}}
	want := "execution-scoped-cancellation-v1,acp-harness-v1:claude,acp-harness-v1:codex"
	if got := w.capabilityHeader(); got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
}
