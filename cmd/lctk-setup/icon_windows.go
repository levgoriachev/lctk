//go:build windows

package main

import "fmt"

const (
	applicationIconResourceID = 1
	imageIcon                 = 1
	smCXIcon                  = 11
	smCYIcon                  = 12
	smCXSmallIcon             = 49
	smCYSmallIcon             = 50
)

var (
	procLoadImageW  = user32.NewProc("LoadImageW")
	procDestroyIcon = user32.NewProc("DestroyIcon")
)

// loadApplicationIcons loads DPI-appropriate handles from the same group-icon
// PE resource that Windows Explorer, Start Menu, and Apps & Features display.
// The returned handles belong to the registered window classes for the rest of
// this short-lived GUI process and are released by Windows at process exit.
func loadApplicationIcons(instance uintptr) (uintptr, uintptr, error) {
	large, err := loadApplicationIcon(instance, int32(systemMetric(smCXIcon)), int32(systemMetric(smCYIcon)))
	if err != nil {
		return 0, 0, fmt.Errorf("load large LCTK application icon: %w", err)
	}
	small, err := loadApplicationIcon(instance, int32(systemMetric(smCXSmallIcon)), int32(systemMetric(smCYSmallIcon)))
	if err != nil {
		procDestroyIcon.Call(large)
		return 0, 0, fmt.Errorf("load small LCTK application icon: %w", err)
	}
	return large, small, nil
}

// loadApplicationIcon asks Windows to select and scale the best frame from the
// embedded multi-size group rather than decoding a separate loose icon file.
func loadApplicationIcon(instance uintptr, width, height int32) (uintptr, error) {
	if width <= 0 || height <= 0 {
		return 0, fmt.Errorf("invalid icon metrics %dx%d", width, height)
	}
	handle, _, callErr := procLoadImageW.Call(instance, applicationIconResourceID, imageIcon, uintptr(width), uintptr(height), 0)
	if handle == 0 {
		return 0, callErr
	}
	return handle, nil
}

// systemMetric keeps every metric read explicit and prevents a failed Win32
// query from being converted into an enormous unsigned LoadImage dimension.
func systemMetric(metric int32) int {
	value, _, _ := procGetSystemMetrics.Call(uintptr(metric))
	return int(int32(value))
}
