//go:build !windows

package uninstall

import (
	"context"
	"errors"
)

func managedDistributionAbsent(context.Context) (bool, error) {
	return false, errors.New("managed WSL verification requires Windows")
}
