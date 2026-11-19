//go:build windows

package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestWindowsHookCancellationKillsProcessTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "processes.json")
	script := filepath.Join(dir, "long running hook.ps1")
	if err := os.WriteFile(script, []byte(`
$child = Start-Process powershell.exe -ArgumentList "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 60" -PassThru
[IO.File]::WriteAllText($env:HOOK_PID_FILE, (@{ Parent = $PID; Child = $child.Id } | ConvertTo-Json))
Start-Sleep -Seconds 60
`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := newHookCommand(ctx,
		`powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "`+script+`"`,
		dir, []string{"HOOK_PID_FILE=" + pidFile}, nil)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	})

	var pids struct{ Parent, Child uint32 }
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil && json.Unmarshal(data, &pids) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hook did not report its descendant process IDs")
		}
		select {
		case err := <-done:
			t.Fatalf("hook exited before cancellation: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	var handles []syscall.Handle
	for _, pid := range []uint32{pids.Parent, pids.Child} {
		handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE|syscall.PROCESS_TERMINATE, false, pid)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = syscall.TerminateProcess(handle, 1)
			_ = syscall.CloseHandle(handle)
		})
		status, err := syscall.WaitForSingleObject(handle, 0)
		if err != nil || status != syscall.WAIT_TIMEOUT {
			t.Fatalf("descendant %d was not running before cancellation: status=%d error=%v", pid, status, err)
		}
		handles = append(handles, handle)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled hook returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled hook did not exit")
	}
	for _, handle := range handles {
		status, err := syscall.WaitForSingleObject(handle, 5000)
		if err != nil || status != syscall.WAIT_OBJECT_0 {
			t.Fatalf("descendant survived cancellation: status=%d error=%v", status, err)
		}
	}
}
