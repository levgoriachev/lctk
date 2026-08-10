// Package machinetunnel exposes container services on Windows loopback through
// the authenticated SSH channel of LCTK's private Podman machine.
//
// WSL localhost forwarding is shared by every distribution. When Docker
// Desktop and LCTK publish the same Linux-side port, wslrelay can select the
// wrong distribution and accept a connection that never reaches LCTK. An SSH
// tunnel is scoped to the exact managed machine and avoids that global relay.
package machinetunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"golang.org/x/crypto/ssh"
)

// remoteClient is the bounded part of ssh.Client needed by one forwarding
// registry. Keeping it as an interface makes byte forwarding testable without
// creating a real managed machine.
type remoteClient interface {
	Dial(string, string) (net.Conn, error)
	SendRequest(string, bool, []byte) (bool, []byte, error)
	Close() error
}

type opener func(context.Context) (remoteClient, error)

const machineOpenTimeout = 15 * time.Second

type forward struct {
	listener net.Listener
	client   remoteClient
	remote   string
}

// Registry owns process-local loopback listeners keyed by product component.
// Reusing a key preserves stable addresses while a daemon is alive; a changed
// remote port or restarted SSH machine atomically replaces the stale tunnel.
type Registry struct {
	mu      sync.Mutex
	open    opener
	forward map[string]*forward
}

// Default is the production registry backed by the exact lctk-runtime SSH
// identity reported by the verified private Podman client.
var Default = New(openMachineClient)

// New constructs a forwarding registry around one authenticated SSH opener.
func New(open opener) *Registry {
	return &Registry{open: open, forward: make(map[string]*forward)}
}

// Ensure returns a Windows loopback address forwarding to one numeric private
// address inside lctk-runtime. The remote boundary rejects public addresses and
// hostnames so a caller cannot turn this product service into an arbitrary SSH
// proxy or redirect it through machine-controlled DNS.
func (r *Registry) Ensure(ctx context.Context, key, remote string) (string, error) {
	if strings.TrimSpace(key) == "" || r == nil || r.open == nil {
		return "", errors.New("machine tunnel is incomplete")
	}
	host, port, err := net.SplitHostPort(remote)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsPrivate() && !ip.IsLoopback() {
		return "", fmt.Errorf("machine tunnel remote must be a numeric private address: %q", remote)
	}
	if value, parseErr := strconv.ParseUint(port, 10, 16); parseErr != nil || value == 0 {
		return "", fmt.Errorf("machine tunnel remote port is invalid: %q", remote)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.forward[key]; current != nil && current.remote == remote {
		if _, _, keepaliveErr := current.client.SendRequest("keepalive@openssh.com", true, nil); keepaliveErr == nil {
			return current.listener.Addr().String(), nil
		}
		r.closeLocked(key)
	} else if current != nil {
		r.closeLocked(key)
	}
	// Machine discovery and SSH authentication are implementation details of a
	// local GUI action. Bound them independently from the caller so a broken WSL
	// relay or an SSH prompt can never leave the window waiting indefinitely.
	openContext, cancelOpen := context.WithTimeout(ctx, machineOpenTimeout)
	client, err := r.open(openContext)
	cancelOpen()
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = client.Close()
		return "", fmt.Errorf("open machine tunnel loopback listener: %w", err)
	}
	created := &forward{listener: listener, client: client, remote: remote}
	r.forward[key] = created
	go serve(created)
	return listener.Addr().String(), nil
}

// Close releases one component listener without affecting other project
// tunnels or the managed Podman machine itself.
func (r *Registry) Close(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked(key)
}

func (r *Registry) closeLocked(key string) {
	current := r.forward[key]
	if current == nil {
		return
	}
	delete(r.forward, key)
	_ = current.listener.Close()
	_ = current.client.Close()
}

func serve(current *forward) {
	for {
		local, err := current.listener.Accept()
		if err != nil {
			return
		}
		go proxy(local, current.client, current.remote)
	}
}

func proxy(local net.Conn, client remoteClient, remoteAddress string) {
	defer local.Close()
	remote, err := client.Dial("tcp", remoteAddress)
	if err != nil {
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		done <- struct{}{}
	}()
	<-done
	_ = local.Close()
	_ = remote.Close()
	<-done
}

type machineInspection struct {
	SSHConfig struct {
		IdentityPath   string `json:"IdentityPath"`
		Port           int    `json:"Port"`
		RemoteUsername string `json:"RemoteUsername"`
	} `json:"SSHConfig"`
}

func openMachineClient(ctx context.Context) (remoteClient, error) {
	inspectionOutput, err := runMachine(ctx, "inspect", containerruntime.MachineName)
	if err != nil {
		return nil, err
	}
	var inspections []machineInspection
	if err := json.Unmarshal(inspectionOutput, &inspections); err != nil {
		return nil, fmt.Errorf("decode managed machine SSH configuration: %w", err)
	}
	if len(inspections) != 1 {
		return nil, fmt.Errorf("managed machine inspection returned %d records", len(inspections))
	}
	config := inspections[0].SSHConfig
	if config.Port <= 0 || config.Port > 65535 || strings.TrimSpace(config.RemoteUsername) == "" {
		return nil, errors.New("managed machine SSH configuration is incomplete")
	}
	identity, err := validatedIdentityPath(config.IdentityPath)
	if err != nil {
		return nil, err
	}
	privateBody, err := os.ReadFile(identity)
	if err != nil {
		return nil, fmt.Errorf("read managed machine SSH identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(privateBody)
	if err != nil {
		return nil, fmt.Errorf("parse managed machine SSH identity: %w", err)
	}
	hostBody, err := runMachine(ctx, "ssh", containerruntime.MachineName, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return nil, err
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey(bytes.TrimSpace(hostBody))
	if err != nil {
		return nil, fmt.Errorf("parse managed machine SSH host key: %w", err)
	}
	sshConfig := &ssh.ClientConfig{
		User:              config.RemoteUsername,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, presented ssh.PublicKey) error {
			if !bytes.Equal(hostKey.Marshal(), presented.Marshal()) {
				return errors.New("managed machine SSH host key changed")
			}
			return nil
		},
		Timeout: 10 * time.Second,
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port)), sshConfig)
	if err != nil {
		return nil, fmt.Errorf("connect authenticated managed machine tunnel: %w", err)
	}
	return client, nil
}

func validatedIdentityPath(identity string) (string, error) {
	data, err := lctkhome.RuntimeDataDir()
	if err != nil {
		return "", err
	}
	wantedRoot := filepath.Join(data, "containers", "podman", "machine")
	absolute, err := filepath.Abs(identity)
	if err != nil {
		return "", fmt.Errorf("resolve managed machine SSH identity: %w", err)
	}
	relative, err := filepath.Rel(wantedRoot, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("managed machine SSH identity is outside runtime data: %q", absolute)
	}
	return absolute, nil
}

func runMachine(ctx context.Context, args ...string) ([]byte, error) {
	command, err := containerruntime.MachineCommand(ctx, args...)
	if err != nil {
		return nil, err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("query managed machine SSH identity: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return output, nil
}
