package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

// runAdminOpen starts the installed native administrator application. The
// daemon session is exchanged inside that process and is never placed in a URL
// or delegated to a browser.
func runAdminOpen(args []string, _ io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: lctk admin open")
	}
	if runtime.GOOS != "windows" {
		return errors.New("the native LCTK administrator window is available only on Windows")
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	command := exec.Command(filepath.Join(home, "bin", "lctk-setup.exe"), "--admin")
	if err := command.Start(); err != nil {
		return fmt.Errorf("open native LCTK administrator window: %w", err)
	}
	return command.Process.Release()
}

func runAdmin(args []string, stdout, stderr io.Writer) error {
	const usage = "Usage:\n  lctk admin open\n"
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("an admin subcommand is required")
	}
	switch args[0] {
	case "open":
		return runAdminOpen(args[1:], stdout)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown admin subcommand %q", strings.TrimSpace(args[0]))
	}
}
