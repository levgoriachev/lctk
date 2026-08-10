package machinetunnel

import (
	"context"
	"io"
	"net"
	"testing"
)

type localClient struct{}

func (localClient) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

func (localClient) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return true, nil, nil
}

func (localClient) Close() error { return nil }

func TestRegistryForwardsLoopbackBytesAndReusesTheComponentAddress(t *testing.T) {
	remote, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	go func() {
		connection, acceptErr := remote.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	registry := New(func(context.Context) (remoteClient, error) { return localClient{}, nil })
	local, err := registry.Ensure(t.Context(), "project-alpha", remote.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	again, err := registry.Ensure(t.Context(), "project-alpha", remote.Addr().String())
	if err != nil || again != local {
		t.Fatalf("reused address=%q err=%v want=%q", again, err, local)
	}
	connection, err := net.Dial("tcp", local)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("proof")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "proof" {
		t.Fatalf("forwarded=%q err=%v", buffer, err)
	}
	registry.Close("project-alpha")
	if _, err := net.Dial("tcp", local); err == nil {
		t.Fatal("closed tunnel still accepted connections")
	}
}

func TestRegistryRejectsNonLoopbackRemote(t *testing.T) {
	registry := New(func(context.Context) (remoteClient, error) { return localClient{}, nil })
	if _, err := registry.Ensure(t.Context(), "unsafe", "192.0.2.1:8080"); err == nil {
		t.Fatal("public machine target was accepted")
	}
}
