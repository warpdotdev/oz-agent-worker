//go:build windows

package worker

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowsDirectHooks(t *testing.T) {
	t.Setenv("HOOK_HOST_VALUE", "inherited")
	t.Setenv("OZ_RUN_ID", "host-run")
	t.Setenv("OZ_ENVIRONMENT_FILE", "")
	dir := t.TempDir()
	scriptDir := filepath.Join(dir, "quoted scripts")
	if err := os.Mkdir(scriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scriptDir, "hook script.ps1")
	if err := os.WriteFile(script, []byte(`
param([string]$Name, [int]$Code)
@{
    OZ_RUN_ID = $env:OZ_RUN_ID
    OZ_WORKSPACE_ROOT = $env:OZ_WORKSPACE_ROOT
    OZ_ENVIRONMENT_FILE = $env:OZ_ENVIRONMENT_FILE
    HOOK_HOST_VALUE = $env:HOOK_HOST_VALUE
    DIRECTORY = (Get-Location).Path
} | ConvertTo-Json | Set-Content -Encoding Ascii "$Name.json"
exit $Code
`), 0600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "environment file")
	backend := &DirectBackend{}
	for _, tc := range []struct {
		name string
		code int
		env  []string
	}{
		{"setup", 0, directSetupEnvVars(dir, "run-123", envFile)},
		{"teardown", 7, directTeardownEnvVars(dir, filepath.Join(dir, ".gitconfig"), "run-123")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "` + script + `" -Name ` + tc.name
			if tc.code != 0 {
				command += " -Code 7"
			}
			err := backend.runCommand(t.Context(), command, dir, tc.env, nil)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.code {
					t.Fatalf("hook error = %v, want exit code %d", err, tc.code)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"OZ_RUN_ID": "run-123", "OZ_WORKSPACE_ROOT": dir,
				"OZ_ENVIRONMENT_FILE": "", "HOOK_HOST_VALUE": "inherited", "DIRECTORY": dir,
			}
			if tc.name == "setup" {
				want["OZ_ENVIRONMENT_FILE"] = envFile
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("hook environment and directory = %#v, want %#v", got, want)
			}
		})
	}
}

func TestWindowsCommandHooks(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "dispatch script.ps1")
	payloadFile := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(script, []byte(`
if ($env:OZ_RUN_ID -ne "task-1" -or $env:OPERATOR_VAR -ne "configured") { exit 10 }
[IO.File]::WriteAllText($env:PAYLOAD_FILE, [Console]::In.ReadToEnd())
`), 0600); err != nil {
		t.Fatal(err)
	}
	backend := newTestCommandBackend(t, CommandBackendConfig{
		DispatchCommand: `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "` + script + `"`,
		CancelCommand:   `if "%OZ_RUN_ID%"=="task-1" (exit /b 7) else (exit /b 10)`,
		Env: map[string]string{
			"OPERATOR_VAR": "configured",
			"PAYLOAD_FILE": payloadFile,
		},
	})
	result := backend.ExecuteTask(t.Context(), testTaskParams())
	if result.Error != nil || result.Outcome != ExecuteOutcomeSpawned {
		t.Fatalf("dispatch result = %+v, want spawned", result)
	}
	data, err := os.ReadFile(payloadFile)
	if err != nil {
		t.Fatal(err)
	}
	var payload DispatchPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RunID != "task-1" {
		t.Fatalf("payload run ID = %q, want task-1", payload.RunID)
	}
	err = backend.CancelTask(t.Context(), &CancelParams{TaskID: "task-1"})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("cancel error = %v, want exit code 7", err)
	}
}

func TestWindowsShellComSpec(t *testing.T) {
	shell, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatal(err)
	}
	for _, comSpec := range []string{shell, ""} {
		t.Run(comSpec, func(t *testing.T) {
			t.Setenv("ComSpec", comSpec)
			cmd := newShellCommand(t.Context(), "exit /b 0")
			if cmd.Path != shell {
				t.Fatalf("shell path = %q, want %q", cmd.Path, shell)
			}
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
