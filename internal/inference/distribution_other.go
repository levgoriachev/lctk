//go:build !windows

package inference

import "os"

// replaceStateFile uses the POSIX same-filesystem atomic replacement.
func replaceStateFile(staged, target string) error {
	return os.Rename(staged, target)
}
