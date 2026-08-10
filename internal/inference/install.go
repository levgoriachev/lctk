package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
)

// ImageAvailable checks the immutable image identity without pulling it.
func (m *Manager) ImageAvailable(ctx context.Context) bool {
	_, _, err := m.runner.Run(ctx, "image", "inspect", m.image, "--format", "{{.Id}}")
	return err == nil
}

// PullImage installs exactly the pinned multi-architecture manifest selected by
// the host runtime. A mutable tag never crosses this boundary.
func (m *Manager) PullImage(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, stderr, err := m.runner.Run(ctx, "pull", m.image); err != nil {
		return fmt.Errorf("pull embedding inference image: %s", firstLine(stderr, err))
	}
	return nil
}

// ModelAvailable verifies content, not just presence.
func (m *Manager) ModelAvailable() bool {
	return verifyFile(m.modelPath, m.modelBytes, m.modelSHA) == nil
}

// InstallModel downloads the pinned model into the installation directory and
// swaps it only after length, digest, fsync, and close all succeed. An existing
// valid model is reused without network access.
func (m *Manager) InstallModel(ctx context.Context, client *http.Client) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.ModelAvailable() {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	dir := filepath.Dir(m.modelPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create model directory %q: %w", dir, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.downloadURL, nil)
	if err != nil {
		return fmt.Errorf("create model download request: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download embedding model: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download embedding model: server returned %s", response.Status)
	}
	temp, err := os.CreateTemp(dir, ".model-*.tmp")
	if err != nil {
		return fmt.Errorf("create model download in %q: %w", dir, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(response.Body, m.modelBytes+1))
	if copyErr != nil {
		temp.Close()
		return fmt.Errorf("write embedding model: %w", copyErr)
	}
	if written != m.modelBytes {
		temp.Close()
		return fmt.Errorf("%w: downloaded %d bytes, want %d", ErrModelInvalid, written, m.modelBytes)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != m.modelSHA {
		temp.Close()
		return fmt.Errorf("%w: downloaded %s, want %s", ErrModelInvalid, got, m.modelSHA)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("flush embedding model: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close embedding model: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("restrict embedding model permissions: %w", err)
	}
	backup := m.modelPath + ".rollback"
	_ = os.Remove(backup)
	hadCurrent := false
	if _, err := os.Stat(m.modelPath); err == nil {
		if err := os.Rename(m.modelPath, backup); err != nil {
			return fmt.Errorf("prepare embedding model replacement: %w", err)
		}
		hadCurrent = true
	}
	if err := os.Rename(tempPath, m.modelPath); err != nil {
		if hadCurrent {
			_ = os.Rename(backup, m.modelPath)
		}
		return fmt.Errorf("activate embedding model: %w", err)
	}
	if err := verifyFile(m.modelPath, m.modelBytes, m.modelSHA); err != nil {
		_ = os.Remove(m.modelPath)
		if hadCurrent {
			_ = os.Rename(backup, m.modelPath)
		}
		return err
	}
	if hadCurrent {
		_ = os.Remove(backup)
	}
	return nil
}

// SelfTest proves the loaded model answers the actual OpenAI-compatible client
// path and returns the pinned dimension with finite values.
func (m *Manager) SelfTest(ctx context.Context) error {
	return m.selfTestFor(ctx, ContainerName)
}

func (m *Manager) selfTestFor(ctx context.Context, name string) error {
	body, err := json.Marshal(map[string]any{
		"model": ModelAlias, "input": []string{"search_query: locate retry backoff logic"},
		"encoding_format": "float",
	})
	if err != nil {
		return err
	}
	address, err := m.serviceAddressFor(ctx, name)
	if err != nil {
		return fmt.Errorf("resolve embedding self-test endpoint: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := m.selfTestClient.Do(request)
	if err != nil {
		return fmt.Errorf("embedding self-test request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("embedding self-test returned %s", response.Status)
	}
	var decoded struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&decoded); err != nil {
		return fmt.Errorf("decode embedding self-test: %w", err)
	}
	if len(decoded.Data) != 1 || len(decoded.Data[0].Embedding) != Dimensions {
		return fmt.Errorf("embedding self-test returned the wrong vector count or dimension")
	}
	for _, value := range decoded.Data[0].Embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("embedding self-test returned a non-finite value")
		}
	}
	return nil
}
