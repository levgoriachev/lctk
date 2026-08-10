// Package daemonstate records, starts, and safely stops only the verified LCTK
// daemon process during setup, update, and uninstall.
package daemonstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const FileName = "daemon.json"

type State struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

// Register atomically records the current daemon identity and returns cleanup.
func Register(home string) (func(), error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	state := State{PID: os.Getpid(), Executable: filepath.Clean(executable)}
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	target := filepath.Join(home, FileName)
	temporary, err := os.CreateTemp(home, "daemon.*.tmp")
	if err != nil {
		return nil, err
	}
	name := temporary.Name()
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		os.Remove(name)
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return nil, err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return nil, err
	}
	return func() { _ = os.Remove(target) }, nil
}

// Load validates that the recorded executable belongs beneath the installation
// home before a platform implementation may inspect the process.
func Load(home string) (State, error) {
	body, err := os.ReadFile(filepath.Join(home, FileName))
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(body, &state); err != nil {
		return State{}, fmt.Errorf("decode daemon state: %w", err)
	}
	relative, err := filepath.Rel(home, state.Executable)
	if err != nil || state.PID <= 0 || filepath.IsAbs(relative) || relative == ".." || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return State{}, errors.New("daemon state is outside the installation home")
	}
	return state, nil
}

// Stop terminates the recorded process only after the OS confirms its executable
// still matches the installation-owned identity.
func Stop(home string) error { return stop(home) }

// Start launches the stable installation-owned daemon without exposing a
// terminal window. Platform code owns the detach semantics.
func Start(home string) error { return start(home) }
