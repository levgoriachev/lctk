//go:build !windows

package daemonstate

import "errors"

// start is unavailable outside the Windows installed-product boundary.
func start(string) error { return errors.New("managed daemon start currently supports Windows only") }
