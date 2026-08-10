// release-manifest creates the one signed component inventory consumed by
// bootstrap and update. It is a release-workflow tool, not part of the product.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

type artifactFlags []string

func (values *artifactFlags) String() string { return strings.Join(*values, ";") }
func (values *artifactFlags) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	var artifacts artifactFlags
	version := flag.String("version", "", "release version")
	commit := flag.String("commit", "", "release commit")
	published := flag.String("published-at", "", "RFC3339 publication time")
	baseURL := flag.String("base-url", "", "release download base URL")
	codeImage := flag.String("code-image", "", "immutable code-intel image reference")
	codeBytes := flag.Int64("code-image-bytes", 0, "compressed code-intel bytes")
	inferenceBytes := flag.Int64("inference-image-bytes", 0, "compressed inference image bytes")
	nvidiaInferenceBytes := flag.Int64("nvidia-inference-image-bytes", 0, "compressed NVIDIA inference image bytes")
	nvidiaInferenceUnpackedBytes := flag.Int64("nvidia-inference-image-unpacked-bytes", 0, "unpacked NVIDIA inference layer bytes")
	keyID := flag.String("key-id", "", "release signing key identifier")
	output := flag.String("output", "", "signed envelope output path")
	printPublic := flag.Bool("print-public-key", false, "print the release public key and exit")
	templateEnvelope := flag.String("template-envelope", "", "verified signed manifest used as a local RC component template")
	templateKeyID := flag.String("template-key-id", "", "trusted key id for --template-envelope")
	templatePublicKey := flag.String("template-public-key", "", "base64 Ed25519 public key for --template-envelope")
	flag.Var(&artifacts, "artifact", "name,kind,os,arch,path; repeat for every artifact")
	flag.Parse()

	private, err := privateKeyFromEnvironment()
	if err != nil {
		fatal(err)
	}
	if *printPublic {
		fmt.Print(base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)))
		return
	}
	if *version == "" || *commit == "" || *published == "" || *baseURL == "" || *keyID == "" || *output == "" {
		fatal(errors.New("complete release identity, image sizes, key id, output, and artifacts are required"))
	}
	if _, err := time.Parse(time.RFC3339, *published); err != nil {
		fatal(fmt.Errorf("published-at: %w", err))
	}
	if *nvidiaInferenceBytes != nvidiainstall.ImageCompressedBytes ||
		*nvidiaInferenceUnpackedBytes != nvidiainstall.ImageUnpackedBytes {
		fatal(errors.New("NVIDIA inference compressed and unpacked sizes must match the pinned CUDA image"))
	}
	parsedArtifacts, err := loadArtifacts(artifacts, strings.TrimRight(*baseURL, "/"))
	if err != nil {
		fatal(err)
	}
	var manifest releasebundle.Manifest
	if *templateEnvelope != "" {
		manifest, err = loadTemplate(*templateEnvelope, *templateKeyID, *templatePublicKey)
		if err != nil {
			fatal(err)
		}
		manifest.Version, manifest.Commit, manifest.PublishedAt = *version, *commit, *published
		manifest.Artifacts = replaceArtifacts(manifest.Artifacts, parsedArtifacts)
		if *codeImage != "" {
			if *codeBytes <= 0 {
				fatal(errors.New("code image bytes are required with a local code image"))
			}
			codeDigest, digestErr := imageDigest(*codeImage)
			if digestErr != nil {
				fatal(digestErr)
			}
			manifest.CodeImage = releasebundle.Image{
				Name: "code-intel", Reference: *codeImage, Digest: codeDigest,
				CompressedBytes: *codeBytes, Platforms: []string{"linux/amd64"},
			}
		}
		manifest.NVIDIAGPUInferenceImage = nvidiaImage()
	} else {
		if *codeImage == "" || *codeBytes <= 0 || *inferenceBytes <= 0 {
			fatal(errors.New("code and inference image sizes are required without --template-envelope"))
		}
		codeDigest, digestErr := imageDigest(*codeImage)
		if digestErr != nil {
			fatal(digestErr)
		}
		inferenceDigest, digestErr := imageDigest(inference.Image)
		if digestErr != nil {
			fatal(digestErr)
		}
		manifest = releasebundle.Manifest{
			SchemaVersion:      releasebundle.SchemaVersion,
			Version:            *version,
			Commit:             *commit,
			PublishedAt:        *published,
			MinimumHostVersion: "0.1.0",
			ProjectSchemaFrom:  1,
			ProjectSchemaTo:    2,
			Artifacts:          parsedArtifacts,
			CodeImage: releasebundle.Image{
				Name: "code-intel", Reference: *codeImage, Digest: codeDigest,
				CompressedBytes: *codeBytes, Platforms: []string{"linux/amd64", "linux/arm64"},
			},
			InferenceImage: releasebundle.Image{
				Name: "embedding-inference", Reference: inference.Image, Digest: inferenceDigest,
				CompressedBytes: *inferenceBytes, Platforms: []string{"linux/amd64", "linux/arm64"},
			},
			NVIDIAGPUInferenceImage: nvidiaImage(),
			EmbeddingModel: releasebundle.Model{
				Name: inference.ModelName, URL: inference.ModelURL, Bytes: inference.ModelBytes,
				SHA256: inference.ModelSHA256, License: "Apache-2.0",
			},
			MigrationNotesURL:    strings.TrimRight(*baseURL, "/") + "/migration-notes.md",
			RollbackInstructions: "Run `lctk update rollback`; see migration-notes.md for state and manual recovery evidence.",
		}
	}
	if err := manifest.Validate(); err != nil {
		fatal(err)
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		fatal(err)
	}
	envelope := releasebundle.Envelope{
		KeyID: *keyID, Algorithm: "ed25519",
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload)),
	}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(*output, encoded, 0o600); err != nil {
		fatal(err)
	}
}

// nvidiaImage centralizes the immutable CUDA identity so official and local
// manifests cannot diverge from the package that validates and runs it.
func nvidiaImage() releasebundle.Image {
	return releasebundle.Image{
		Name: "embedding-inference-nvidia-gpu", Reference: nvidiainstall.Image,
		Digest: nvidiainstall.ImageDigest, CompressedBytes: nvidiainstall.ImageCompressedBytes,
		UnpackedBytes: nvidiainstall.ImageUnpackedBytes,
		Platforms:     []string{"linux/amd64", "linux/arm64"},
	}
}

// loadTemplate authenticates the official manifest before any remote runtime,
// image, or model identity can enter a locally signed release candidate.
func loadTemplate(path, keyID, encodedPublicKey string) (releasebundle.Manifest, error) {
	if path == "" || keyID == "" || encodedPublicKey == "" {
		return releasebundle.Manifest{}, errors.New("template envelope, key id, and public key are required together")
	}
	publicKey, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return releasebundle.Manifest{}, errors.New("template public key is invalid")
	}
	document, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return releasebundle.Manifest{}, fmt.Errorf("read template envelope: %w", err)
	}
	return (releasebundle.Verifier{KeyID: keyID, PublicKey: ed25519.PublicKey(publicKey)}).Verify(document)
}

// replaceArtifacts keeps the template's verified runtime inventory and swaps
// only identities explicitly built for the local release candidate.
func replaceArtifacts(template, replacements []releasebundle.Artifact) []releasebundle.Artifact {
	byIdentity := make(map[string]releasebundle.Artifact, len(replacements))
	for _, artifact := range replacements {
		byIdentity[artifact.Kind+"\x00"+artifact.OS+"\x00"+artifact.Arch] = artifact
	}
	result := make([]releasebundle.Artifact, 0, len(template)+len(replacements))
	for _, artifact := range template {
		identity := artifact.Kind + "\x00" + artifact.OS + "\x00" + artifact.Arch
		if replacement, ok := byIdentity[identity]; ok {
			result = append(result, replacement)
			delete(byIdentity, identity)
		} else {
			result = append(result, artifact)
		}
	}
	for _, artifact := range replacements {
		identity := artifact.Kind + "\x00" + artifact.OS + "\x00" + artifact.Arch
		if _, ok := byIdentity[identity]; ok {
			result = append(result, artifact)
			delete(byIdentity, identity)
		}
	}
	return result
}

func privateKeyFromEnvironment() (ed25519.PrivateKey, error) {
	encoded := os.Getenv("LCTK_RELEASE_ED25519_PRIVATE_KEY")
	if encoded == "" {
		return nil, errors.New("LCTK_RELEASE_ED25519_PRIVATE_KEY is required")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("release private key is not valid base64")
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	default:
		return nil, fmt.Errorf("release private key has %d bytes, want %d or %d", len(decoded), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

func loadArtifacts(specifications []string, baseURL string) ([]releasebundle.Artifact, error) {
	artifacts := make([]releasebundle.Artifact, 0, len(specifications))
	for _, specification := range specifications {
		fields := strings.Split(specification, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("artifact %q must have name,kind,os,arch,path", specification)
		}
		name, kind, targetOS, arch, path := fields[0], fields[1], fields[2], fields[3], fields[4]
		size, digest, err := fileIdentity(path)
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", name, err)
		}
		artifacts = append(artifacts, releasebundle.Artifact{
			Name: name, Kind: kind, OS: targetOS, Arch: arch,
			URL: baseURL + "/" + name, Bytes: size, SHA256: digest,
		})
	}
	return artifacts, nil
}

func fileIdentity(path string) (int64, string, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func imageDigest(reference string) (string, error) {
	_, digest, found := strings.Cut(reference, "@sha256:")
	if !found || len(digest) != 64 {
		return "", fmt.Errorf("image %q has no sha256 digest", reference)
	}
	if _, err := strconv.ParseUint(digest[:16], 16, 64); err != nil {
		return "", fmt.Errorf("image %q has an invalid digest", reference)
	}
	return "sha256:" + digest, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "release-manifest:", err)
	os.Exit(1)
}
