//go:build !windows

package main

import "errors"

func runSetupWindow(setupRequest) error {
	return errors.New("the native LCTK setup window is available only on Windows")
}
