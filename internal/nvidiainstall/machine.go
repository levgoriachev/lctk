package nvidiainstall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
)

// machineTransport enters only the LCTK-owned machine as its configured root
// user. Every remote executable and script is supplied as an argument; no host
// shell or ambient WSL distribution participates.
type machineTransport struct{}

func (machineTransport) Run(ctx context.Context, args ...string) (string, string, error) {
	return runMachine(ctx, nil, args...)
}

func (machineTransport) RunInput(ctx context.Context, input io.Reader, args ...string) (string, string, error) {
	return runMachine(ctx, input, args...)
}

func runMachine(ctx context.Context, input io.Reader, args ...string) (string, string, error) {
	remote, err := remoteCommand(args)
	if err != nil {
		return "", "", err
	}
	// Podman 5.8.2 assembles COMMAND [ARG ...] into one remote shell command.
	// Supply one fully POSIX-quoted argument so spaces, newlines, semicolons,
	// and embedded quotes cannot change the fixed argv boundaries.
	selected := []string{"ssh", containerruntime.MachineName, remote}
	command, err := containerruntime.MachineCommand(ctx, selected...)
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	command.Stdin = input
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	return stdout.String(), stderr.String(), err
}

func remoteCommand(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("NVIDIA machine command is empty")
	}
	quoted := make([]string, len(args))
	for index, arg := range args {
		// A single quote inside a POSIX single-quoted word is represented by
		// closing the word, emitting one quote, and opening it again.
		quoted[index] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " "), nil
}
