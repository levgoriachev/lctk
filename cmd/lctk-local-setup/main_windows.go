//go:build windows

// lctk-local-setup is a self-extracting local release-candidate harness. A ZIP
// payload is appended to this executable after build; the harness exposes only
// those verified candidate files on numeric loopback while the real native
// setup performs its normal signed-manifest transaction.
package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const localPackageAddress = "127.0.0.1:4466"

// DefaultSetupMode is empty for the complete installer and is set to
// "uninstall" only by the local recovery build. Keeping the mode in the native
// wrapper lets a partially removed installation run the fixed uninstaller by
// double-click without replacing installed files or opening a terminal.
var DefaultSetupMode string

var payloadNames = map[string]bool{
	"release-manifest.json": true,
	"lctk-core.exe":         true,
	"lctk.exe":              true,
	"lctk-setup.exe":        true,
	"lctk-code-intel.tar":   true,
	"nvidia-container-toolkit-base-1.19.1-1.x86_64.rpm": true,
}

func main() {
	if err := run(); err != nil {
		message, _ := windows.UTF16PtrFromString(err.Error())
		title, _ := windows.UTF16PtrFromString("LCTK Local Setup")
		_, _ = windows.MessageBox(0, message, title, windows.MB_OK|windows.MB_ICONERROR)
		os.Exit(1)
	}
}

func run() (runErr error) {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate local setup: %w", err)
	}
	directory := filepath.Join(filepath.Dir(self), fmt.Sprintf(".lctk-local-setup-%d", os.Getpid()))
	if err := os.Mkdir(directory, 0o700); err != nil {
		return fmt.Errorf("create local setup workspace: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(directory); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("remove local setup workspace: %w", err))
		}
	}()
	if err := extractPayload(self, directory); err != nil {
		return err
	}
	manifest := filepath.Join(directory, "release-manifest.json")
	setup := filepath.Join(directory, "lctk-setup.exe")
	arguments, err := setupArguments(manifest)
	if err != nil {
		return err
	}
	command := exec.Command(setup, arguments...)
	// The embedded setup is a GUI-subsystem executable. Do not apply HideWindow:
	// Windows uses that startup flag for the application's first ShowWindow call
	// and would hide the real setup or uninstall surface together with a console.
	if DefaultSetupMode == "uninstall" {
		// Recovery needs only the extracted native uninstaller. It must not bind
		// the setup artifact endpoint because an unrelated listener must never
		// prevent cleanup of an already partially removed installation.
		if err := command.Run(); err != nil {
			return fmt.Errorf("native recovery uninstaller failed: %w", err)
		}
		return nil
	}
	listener, err := net.Listen("tcp", localPackageAddress)
	if err != nil {
		return fmt.Errorf("open local package endpoint %s: %w", localPackageAddress, err)
	}
	server := &http.Server{Handler: packageHandler(directory), ReadHeaderTimeout: 5 * time.Second}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	if err := command.Start(); err != nil {
		return fmt.Errorf("start native release-candidate setup: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("native release-candidate setup failed: %w", err)
		}
		return nil
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			_ = command.Process.Kill()
			<-done
			return fmt.Errorf("serve local release-candidate package: %w", err)
		}
		return nil
	}
}

// setupArguments limits the compile-time local wrapper mode to the two exact
// native setup entry points used by acceptance and recovery.
func setupArguments(manifest string) ([]string, error) {
	switch DefaultSetupMode {
	case "":
		return []string{"--manifest", manifest}, nil
	case "uninstall":
		return []string{"--uninstall"}, nil
	default:
		return nil, fmt.Errorf("unsupported local setup mode %q", DefaultSetupMode)
	}
}

func extractPayload(executable, directory string) error {
	file, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		return errors.New("local setup has no valid appended release-candidate payload")
	}
	extracted := map[string]bool{}
	for _, entry := range archive.File {
		name := filepath.ToSlash(entry.Name)
		if !payloadNames[name] || strings.Contains(name, "/") || extracted[name] {
			return fmt.Errorf("local setup payload contains unexpected or duplicate entry %q", name)
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		target := filepath.Join(directory, name)
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		extracted[name] = true
	}
	for name := range payloadNames {
		if !extracted[name] {
			return fmt.Errorf("local setup payload omits %q", name)
		}
	}
	return nil
}

func packageHandler(directory string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(request.URL.Path, "/")
		if !payloadNames[name] {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(writer, request, filepath.Join(directory, name))
	})
}
