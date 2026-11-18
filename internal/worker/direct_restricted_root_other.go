//go:build !unix

package worker

import "errors"

func checkedRestrictedWorkspaceRoot(string) (string, error) {
	return "", errors.New("cannot verify restricted workspace ownership on this platform")
}
