//go:build windows

package main

import "testing"

// TestApplicationIconsLoadFromExecutableResource proves that ordinary Go test
// linking included the same PE resource that the release build will consume.
func TestApplicationIconsLoadFromExecutableResource(t *testing.T) {
	instance, _, callErr := procGetModuleHandleW.Call(0)
	if instance == 0 {
		t.Fatalf("get test module handle: %v", callErr)
	}
	large, small, err := loadApplicationIcons(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		procDestroyIcon.Call(large)
		procDestroyIcon.Call(small)
	})
}
