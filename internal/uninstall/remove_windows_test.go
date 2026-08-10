//go:build windows

package uninstall

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestContainsPathRejectsSiblingWithTheSamePrefix(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lctk")
	inside := filepath.Join(root, "bin", "lctk-setup.exe")
	sibling := filepath.Join(filepath.Dir(root), "lctk-other", "lctk-setup.exe")
	if matched, err := containsPath(root, inside); err != nil || !matched {
		t.Fatalf("inside matched=%t err=%v", matched, err)
	}
	if matched, err := containsPath(root, sibling); err != nil || matched {
		t.Fatalf("sibling matched=%t err=%v", matched, err)
	}
}

func TestDeferredRemovalCommandIsHiddenAndBindsExactTargets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "installed LCTK")
	command, err := deferredRemovalCommand(root, 4242)
	if err != nil {
		t.Fatal(err)
	}
	environment := strings.Join(command.Env, "\n")
	if !strings.Contains(environment, "LCTK_REMOVE_ROOT="+root) || !strings.Contains(environment, "LCTK_REMOVE_PARENT=4242") {
		t.Fatalf("cleanup environment does not bind exact targets: %s", environment)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("cleanup process is not hidden: %+v", command.SysProcAttr)
	}
}
