//go:build !windows

// Package windowsprocess keeps platform-neutral command construction shared
// while Windows alone needs explicit console-window suppression.
package windowsprocess

import "os/exec"

// HideConsole is intentionally a no-op outside Windows, where CREATE_NO_WINDOW
// has no equivalent and these commands already inherit no graphical console.
func HideConsole(_ *exec.Cmd) {}
