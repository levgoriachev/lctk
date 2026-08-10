//go:build !windows

package main

import (
	"context"
	"errors"
)

func runAdminWindow(context.Context, string) error {
	return errors.New("the native LCTK administrator window is available only on Windows")
}
