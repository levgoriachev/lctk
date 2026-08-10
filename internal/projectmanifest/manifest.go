// Package projectmanifest reads the safe project declaration that a repository
// may track in Git, plus an untracked local override.
//
// The manifest is untrusted input written by whoever controls the repository.
// Per docs/security.md it may declare a profile, excludes, language and tooling
// preferences, index settings, and proposed build, test, and lint commands. It
// may not mount host directories, grant capabilities, or supply secrets, and it
// can never determine the authoritative host path.
//
// That last guarantee is structural rather than enforced by a check: Manifest has
// no field capable of holding a host path, so there is nothing for the registry
// to read even if a manifest tries to declare one. Attempts are additionally
// rejected at parse time so the user learns the declaration was ignored.
package projectmanifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// FileName is the tracked manifest, safe to commit.
	FileName = ".mcp-project.yaml"
	// LocalFileName is the untracked override. The repository .gitignore pattern
	// *.local.yaml keeps it out of Git.
	LocalFileName = ".mcp-project.local.yaml"
	// SchemaVersion is the manifest schema this build understands.
	SchemaVersion = 1
	// maxSize bounds the file so a hostile or accidental multi-gigabyte manifest
	// cannot exhaust memory during registration.
	maxSize = 1 << 20
)

// forbiddenKeys are declarations that would let a repository escalate its own
// privileges or relocate the project. They are rejected wherever they appear.
var forbiddenKeys = map[string]string{
	"path":            "the authoritative host path comes from the registry",
	"host_path":       "the authoritative host path comes from the registry",
	"root":            "the authoritative host path comes from the registry",
	"repository_root": "the authoritative host path comes from the registry",
	"workspace":       "the authoritative host path comes from the registry",
	"mount":           "a manifest cannot mount host directories",
	"mounts":          "a manifest cannot mount host directories",
	"volume":          "a manifest cannot mount host directories",
	"volumes":         "a manifest cannot mount host directories",
	"grant":           "client authorization is owner-approved by LCTK and stored outside Git",
	"grants":          "client authorization is owner-approved by LCTK and stored outside Git",
	"token":           "a manifest cannot contain credentials",
	"tokens":          "a manifest cannot contain credentials",
	"secret":          "a manifest cannot contain credentials",
	"secrets":         "a manifest cannot contain credentials",
	"credentials":     "a manifest cannot contain credentials",
	"capability":      "capabilities are host policy, not repository policy",
	"capabilities":    "capabilities are host policy, not repository policy",
	"admin":           "a manifest cannot grant administrative access",
	"docker":          "container administration is not exposed to a manifest",
	"privileged":      "a manifest cannot request privileged execution",
	"network_mode":    "the network policy is host policy; use network instead",
	"project_id":      "the project identity is derived by LCTK from the host path",
}

// ErrForbiddenField reports a manifest declaration that a repository is not
// allowed to make.
var ErrForbiddenField = errors.New("manifest field is not allowed")

// Index holds indexing preferences.
type Index struct {
	MaxFileSizeKB    int  `yaml:"max_file_size_kb"`
	IncludeGenerated bool `yaml:"include_generated"`
	// DebounceMS proposes how long to wait after the last save before updating
	// the index. A repository may know its own editing shape better than the host
	// default does. It is a proposal: the host clamps it into the range in
	// hostsettings, so a manifest cannot ask for either a busy loop or a window
	// long enough to make search look broken.
	DebounceMS int `yaml:"debounce_ms"`
}

// Commands holds proposed project commands. They are proposals only: per
// docs/product.md the user confirms a command before it becomes runnable policy,
// and nothing here is executed during registration.
type Commands struct {
	Build string `yaml:"build"`
	Test  string `yaml:"test"`
	Lint  string `yaml:"lint"`
}

// Manifest is the safe subset of a project declaration.
//
// Note the absence of any path, mount, grant, or capability field. That absence
// is the guarantee that a manifest cannot relocate or escalate a project.
type Manifest struct {
	SchemaVersion int      `yaml:"schema_version"`
	Profile       string   `yaml:"profile"`
	Excludes      []string `yaml:"excludes"`
	Languages     []string `yaml:"languages"`
	Network       string   `yaml:"network"`
	Index         Index    `yaml:"index"`
	Commands      Commands `yaml:"commands"`
}

// Result is what registration learns from a project folder.
type Result struct {
	// Manifest is the merged declaration. It is the zero value when no file
	// exists, which is the normal case.
	Manifest Manifest
	// TrackedPresent and LocalPresent record which files contributed.
	TrackedPresent bool
	LocalPresent   bool
	// Warnings describe declarations that were ignored, such as unknown keys.
	// They are surfaced to the user rather than failing registration, so that a
	// manifest written for a newer LCTK still works.
	Warnings []string
}

// Load reads the tracked manifest and the local override from a project root.
//
// A missing file is not an error. The local override wins field by field, so a
// developer can adjust a committed declaration without editing tracked content.
func Load(projectRoot string) (Result, error) {
	var result Result

	tracked, trackedFound, err := readFile(filepath.Join(projectRoot, FileName))
	if err != nil {
		return Result{}, err
	}
	result.TrackedPresent = trackedFound

	local, localFound, err := readFile(filepath.Join(projectRoot, LocalFileName))
	if err != nil {
		return Result{}, err
	}
	result.LocalPresent = localFound

	if !trackedFound && !localFound {
		return result, nil
	}

	merged := tracked.manifest
	if localFound {
		merged = overlay(merged, local.manifest)
	}
	if err := validate(&merged); err != nil {
		return Result{}, err
	}

	result.Manifest = merged
	result.Warnings = append(append([]string{}, tracked.warnings...), local.warnings...)
	sort.Strings(result.Warnings)
	return result, nil
}

type parsed struct {
	manifest Manifest
	warnings []string
}

func readFile(path string) (parsed, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return parsed{}, false, nil
		}
		return parsed{}, false, fmt.Errorf("read %q: %w", path, err)
	}
	if info.IsDir() {
		return parsed{}, false, fmt.Errorf("%q is a directory, not a manifest", path)
	}
	if info.Size() > maxSize {
		return parsed{}, false, fmt.Errorf("%q is larger than the %d byte manifest limit", path, maxSize)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return parsed{}, false, fmt.Errorf("read %q: %w", path, err)
	}
	manifest, warnings, err := Parse(raw)
	if err != nil {
		return parsed{}, false, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return parsed{manifest: manifest, warnings: warnings}, true, nil
}

// Parse decodes and screens one manifest document, returning the declaration and
// any warnings about declarations that were ignored.
//
// Screening happens before typed decoding: the document is first read as a
// generic mapping so that forbidden declarations are rejected and unknown ones
// reported, whatever nesting they appear at. Parse does not validate the merged
// result; Load does that after applying the local override.
func Parse(raw []byte) (Manifest, []string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Manifest{}, nil, nil
	}

	var generic map[string]any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return Manifest{}, nil, fmt.Errorf("invalid YAML: %w", err)
	}

	if err := screen(generic, ""); err != nil {
		return Manifest{}, nil, err
	}

	var manifest Manifest
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("invalid manifest: %w", err)
	}

	return manifest, unknownKeys(generic), nil
}

// screen walks the document rejecting forbidden keys at any depth, because a
// nested declaration is no more acceptable than a top-level one.
func screen(node any, path string) error {
	switch value := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if reason, forbidden := forbiddenKeys[normalized]; forbidden {
				return fmt.Errorf("%w: %s: %s", ErrForbiddenField, join(path, key), reason)
			}
			if err := screen(value[key], join(path, key)); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range value {
			if err := screen(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// knownTopLevel mirrors the Manifest yaml tags.
var knownTopLevel = map[string]bool{
	"schema_version": true,
	"profile":        true,
	"excludes":       true,
	"languages":      true,
	"network":        true,
	"index":          true,
	"commands":       true,
}

// unknownKeys reports top-level keys this build ignores, so a typo or a
// forward-dated manifest is visible rather than silently dropped.
func unknownKeys(generic map[string]any) []string {
	var warnings []string
	for key := range generic {
		if !knownTopLevel[strings.ToLower(strings.TrimSpace(key))] {
			warnings = append(warnings, fmt.Sprintf("ignored unknown manifest field %q", key))
		}
	}
	sort.Strings(warnings)
	return warnings
}

// overlay applies the local override on top of the tracked manifest, replacing
// only fields the override actually sets.
func overlay(base, override Manifest) Manifest {
	merged := base
	if override.SchemaVersion != 0 {
		merged.SchemaVersion = override.SchemaVersion
	}
	if override.Profile != "" {
		merged.Profile = override.Profile
	}
	if override.Excludes != nil {
		merged.Excludes = override.Excludes
	}
	if override.Languages != nil {
		merged.Languages = override.Languages
	}
	if override.Network != "" {
		merged.Network = override.Network
	}
	if override.Index.MaxFileSizeKB != 0 {
		merged.Index.MaxFileSizeKB = override.Index.MaxFileSizeKB
	}
	if override.Index.IncludeGenerated {
		merged.Index.IncludeGenerated = true
	}
	if override.Index.DebounceMS != 0 {
		merged.Index.DebounceMS = override.Index.DebounceMS
	}
	if override.Commands.Build != "" {
		merged.Commands.Build = override.Commands.Build
	}
	if override.Commands.Test != "" {
		merged.Commands.Test = override.Commands.Test
	}
	if override.Commands.Lint != "" {
		merged.Commands.Lint = override.Commands.Lint
	}
	return merged
}

// validate checks the merged declaration and normalizes it.
func validate(m *Manifest) error {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = SchemaVersion
	}
	if m.SchemaVersion > SchemaVersion {
		return fmt.Errorf("manifest schema_version %d is newer than the supported version %d",
			m.SchemaVersion, SchemaVersion)
	}

	m.Profile = strings.ToLower(strings.TrimSpace(m.Profile))
	switch m.Profile {
	case "", "minimal", "full":
	default:
		return fmt.Errorf("unknown profile %q: expected minimal or full", m.Profile)
	}

	m.Network = strings.ToLower(strings.TrimSpace(m.Network))
	switch m.Network {
	case "", "none", "full":
	default:
		return fmt.Errorf("unknown network policy %q: expected none or full", m.Network)
	}

	if m.Index.MaxFileSizeKB < 0 {
		return fmt.Errorf("index.max_file_size_kb must not be negative, got %d", m.Index.MaxFileSizeKB)
	}
	if m.Index.DebounceMS < 0 {
		return fmt.Errorf("index.debounce_ms must not be negative, got %d", m.Index.DebounceMS)
	}

	for i, pattern := range m.Excludes {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return fmt.Errorf("excludes[%d] is empty", i)
		}
		if filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, `\`) {
			return fmt.Errorf("excludes[%d] must be a project-relative pattern with forward slashes, got %q", i, pattern)
		}
		if pattern == ".." || strings.HasPrefix(pattern, "../") || strings.Contains(pattern, "/../") {
			return fmt.Errorf("excludes[%d] must not escape the project root, got %q", i, pattern)
		}
		m.Excludes[i] = pattern
	}

	for i, language := range m.Languages {
		normalized := strings.ToLower(strings.TrimSpace(language))
		if normalized == "" {
			return fmt.Errorf("languages[%d] is empty", i)
		}
		m.Languages[i] = normalized
	}
	return nil
}
