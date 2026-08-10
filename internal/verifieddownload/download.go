// Package verifieddownload installs immutable release artifacts through one
// bounded transport, byte-length, and SHA-256 verification path. Manifest
// validation restricts production transport to HTTPS and local RC transport to
// numeric loopback HTTP before anything reaches this package.
package verifieddownload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

// Download writes artifact to target only after its exact signed identity has
// been verified. Existing partial downloads fail fast instead of being reused.
func Download(ctx context.Context, client *http.Client, artifact releasebundle.Artifact, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", artifact.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s returned %s", artifact.Name, response.Status)
	}
	temporary := target + ".download"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s download: %w", artifact.Name, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, artifact.Bytes+1))
	closeErr := file.Close()
	digest := hex.EncodeToString(hash.Sum(nil))
	if copyErr != nil || closeErr != nil || written != artifact.Bytes || digest != artifact.SHA256 {
		_ = os.Remove(temporary)
		return fmt.Errorf("downloaded %s identity differs from the signed manifest", artifact.Name)
	}
	if err := Activate(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("activate %s download: %w", artifact.Name, err)
	}
	return nil
}

// Activate replaces target with a completely written staged file. The
// platform implementation preserves replacement semantics on Windows, where
// os.Rename does not replace an existing destination.
func Activate(staged, target string) error {
	return replaceFile(staged, target)
}

// Verify confirms an existing file's exact immutable identity.
func Verify(path string, size int64, digest string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		return errors.New("file size differs")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("file digest differs")
	}
	return nil
}
