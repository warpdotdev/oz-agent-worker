//go:build unix

package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func checkedRestrictedWorkspaceRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Resolve parent aliases (notably /tmp on macOS), but never follow a
	// symlink for the workspace root itself.
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	root := filepath.Join(parent, filepath.Base(abs))
	if err := checkRestrictedDirectory(root, true); err != nil {
		return "", err
	}
	return root, nil
}

func checkRestrictedDirectory(path string, workspaceRoot bool) error {
	if parent := filepath.Dir(path); parent != path {
		// Check from the filesystem root down so an untrusted ancestor cannot
		// replace an already checked directory.
		if err := checkRestrictedDirectory(parent, false); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return errors.New("restricted workspace requires a real directory with verifiable ownership")
	}
	owner := int(stat.Uid)
	owned := owner == os.Geteuid()
	writableByOthers := info.Mode().Perm()&0o022 != 0
	// A trusted sticky ancestor such as /tmp prevents unrelated users from
	// renaming the next owned component. The credential root must be stricter.
	if (!owned && (workspaceRoot || owner != 0)) ||
		(writableByOthers && (workspaceRoot || info.Mode()&os.ModeSticky == 0)) {
		return fmt.Errorf("unsafe owner or permissions on restricted workspace directory %q", path)
	}
	return nil
}
