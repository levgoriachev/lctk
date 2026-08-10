package sweexplore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteJSONAtomic publishes a complete JSON artifact with one filesystem
// rename. A crash can therefore leave an unreferenced temporary file, but it
// cannot expose a truncated result as completed campaign evidence.
func WriteJSONAtomic(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON artifact: %w", err)
	}
	return WriteFileAtomic(path, append(body, '\n'), 0o600)
}

// WriteFileAtomic writes within the destination directory so the final rename
// stays on one filesystem. Existing evidence is never overwritten.
func WriteFileAtomic(path string, body []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("artifact already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect artifact destination: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".pending-*")
	if err != nil {
		return fmt.Errorf("create pending artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set pending artifact mode: %w", err)
	}
	if _, err := temporary.Write(body); err != nil {
		return fmt.Errorf("write pending artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync pending artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close pending artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish artifact: %w", err)
	}
	committed = true
	return nil
}

// FileSHA256 returns the digest used by immutable manifests and completion
// receipts to detect any later truncation, replacement, or accidental edit.
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
