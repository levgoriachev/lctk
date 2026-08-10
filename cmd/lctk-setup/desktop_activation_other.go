//go:build !windows

package main

// releaseInstalledAdminExecutable is a Windows file-lock boundary. Other build
// targets have no native Admin executable that setup must release.
func releaseInstalledAdminExecutable(string) error { return nil }
