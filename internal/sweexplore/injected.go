package sweexplore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExpectedTree is the working-tree state one instance is allowed to present to
// an agent. It exists so every gate in the harness asserts against one declared
// state instead of against "git status printed nothing": a mined instance is
// explored clean at its base commit, while an injected instance is explored
// with a defect we authored applied on top of that commit, which is dirty by
// construction.
//
// Digest is empty exactly when the expected state is the pristine checkout, so
// the zero value keeps the historical contract.
type ExpectedTree struct {
	BaseCommit  string
	DefectPatch string
	Digest      string
}

// Injected reports whether this state carries an authored defect.
func (expected ExpectedTree) Injected() bool { return expected.Digest != "" }

// DefectPatchFile is one parsed authored defect. The path set is kept because
// proving a patch is applied is not the same as proving nothing else changed,
// and only the second protects the measurement.
type DefectPatchFile struct {
	Path  string
	Paths map[string]struct{}
}

// LoadDefectPatch reads and parses an authored defect patch. A patch with no
// hunks is refused here rather than producing an instance whose ground truth is
// empty and whose "applied" state is indistinguishable from a clean checkout.
func LoadDefectPatch(path string) (DefectPatchFile, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return DefectPatchFile{}, fmt.Errorf("read defect patch: %w", err)
	}
	hunks, err := ParseUnifiedDiff(string(body))
	if err != nil {
		return DefectPatchFile{}, fmt.Errorf("parse defect patch %s: %w", path, err)
	}
	if len(hunks) == 0 {
		return DefectPatchFile{}, fmt.Errorf("defect patch %s contains no hunks", path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return DefectPatchFile{}, err
	}
	paths := make(map[string]struct{}, len(hunks))
	for _, hunk := range hunks {
		paths[hunk.Path] = struct{}{}
	}
	return DefectPatchFile{Path: absolute, Paths: paths}, nil
}

// ApplyDefect applies an authored defect to an already-materialized clean
// checkout and returns the digest that pins the resulting working tree. It
// refuses a tree that is not clean, because a digest taken over someone else's
// edit would pin that edit as if the harness had produced it.
func ApplyDefect(ctx context.Context, root, patchPath string) (string, error) {
	patch, err := LoadDefectPatch(patchPath)
	if err != nil {
		return "", err
	}
	status, err := defectGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", errors.New("benchmark repository is dirty; refusing to apply a defect patch onto it")
	}
	if _, err := defectGit(ctx, root, "apply", "--whitespace=nowarn", patch.Path); err != nil {
		return "", fmt.Errorf("apply defect patch %s: %w", patch.Path, err)
	}
	if err := verifyDefectConfined(ctx, root, patch); err != nil {
		return "", err
	}
	return DefectDigest(ctx, root)
}

// DefectDigest fingerprints everything an agent could change in the checkout.
// Both Git views are needed and neither is sufficient alone: `git diff HEAD`
// records edited content but is blind to a file the agent created, while
// `git status --untracked-files=all` lists that new file but never shows a
// changed line. Output is normalized for line endings so the same tree digests
// identically regardless of how the machine's Git renders it.
func DefectDigest(ctx context.Context, root string) (string, error) {
	diff, err := defectGit(ctx, root, "diff", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := defectGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(normalizeLineEndings(diff)))
	hash.Write([]byte{0})
	hash.Write([]byte(normalizeLineEndings(status)))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ExpectedWorkingTree resolves what the checkout must look like for one
// instance and proves it is in that state. For a mined instance this is the
// historical pristine check. For an injected instance it proves the tree is
// exactly the base commit plus this patch — the patch reverses cleanly and
// nothing outside it differs — and only then adopts the observed digest as the
// state later gates must find unchanged.
func ExpectedWorkingTree(ctx context.Context, root string, instance Instance) (ExpectedTree, error) {
	expected := ExpectedTree{BaseCommit: instance.BaseCommit}
	if !instance.Injected() {
		return expected, VerifyWorkingTree(ctx, root, expected)
	}
	patch, err := LoadDefectPatch(instance.DefectPatch)
	if err != nil {
		return ExpectedTree{}, err
	}
	if err := verifyHead(ctx, root, instance.BaseCommit); err != nil {
		return ExpectedTree{}, err
	}
	applied, err := defectPatchApplied(ctx, root, patch)
	if err != nil {
		return ExpectedTree{}, err
	}
	if !applied {
		return ExpectedTree{}, fmt.Errorf("defect patch %s is not applied to %s; prepare instance %q before running an arm", patch.Path, root, instance.InstanceID)
	}
	if err := verifyDefectConfined(ctx, root, patch); err != nil {
		return ExpectedTree{}, err
	}
	digest, err := DefectDigest(ctx, root)
	if err != nil {
		return ExpectedTree{}, err
	}
	expected.DefectPatch, expected.Digest = patch.Path, digest
	return expected, nil
}

// VerifyWorkingTree proves the checkout still presents the declared state. It
// replaces VerifyRepository's "status is empty" test with an assertion against
// the state the instance actually declares, and reports the two failures
// separately because they call for opposite responses: a missing patch means
// the instance was never prepared, while a digest mismatch means something
// changed a tree that was prepared correctly.
func VerifyWorkingTree(ctx context.Context, root string, expected ExpectedTree) error {
	if err := verifyHead(ctx, root, expected.BaseCommit); err != nil {
		return err
	}
	if !expected.Injected() {
		status, err := defectGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil {
			return err
		}
		if strings.TrimSpace(status) != "" {
			return errors.New("benchmark repository is not clean")
		}
		return nil
	}
	digest, err := DefectDigest(ctx, root)
	if err != nil {
		return err
	}
	if digest == expected.Digest {
		return nil
	}
	applied, detail, err := defectStillApplied(ctx, root, expected.DefectPatch)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("defect patch %s is no longer applied to the benchmark repository", detail)
	}
	return fmt.Errorf("benchmark working tree digest is %s, want %s", digest, expected.Digest)
}

// defectStillApplied answers whether the defect is present. Reversing the patch
// in check mode is the exact answer; when the caller only carried a digest, a
// clean checkout is still conclusive evidence that the patch is gone, which is
// what separates "nobody prepared this instance" from "something edited a tree
// that was prepared correctly".
func defectStillApplied(ctx context.Context, root, patchPath string) (bool, string, error) {
	if strings.TrimSpace(patchPath) == "" {
		status, err := defectGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil {
			return false, "", err
		}
		return strings.TrimSpace(status) != "", "recorded for this instance", nil
	}
	patch, err := LoadDefectPatch(patchPath)
	if err != nil {
		return false, "", err
	}
	applied, err := defectPatchApplied(ctx, root, patch)
	return applied, patch.Path, err
}

// VerifyDefectState is the injected-instance form of VerifyRepository: HEAD is
// still the pinned base commit and the working tree still digests to the value
// recorded when the defect was applied.
func VerifyDefectState(ctx context.Context, root, baseCommit, expectedDigest string) error {
	if strings.TrimSpace(expectedDigest) == "" {
		return errors.New("expected defect digest is required")
	}
	return VerifyWorkingTree(ctx, root, ExpectedTree{BaseCommit: baseCommit, Digest: expectedDigest})
}

// defectPatchApplied reports whether the working tree already carries the
// patch. Reversing it in check mode is the only test that distinguishes "the
// defect is present" from "some other edit happens to touch the same files".
func defectPatchApplied(ctx context.Context, root string, patch DefectPatchFile) (bool, error) {
	if _, err := defectGit(ctx, root, "apply", "--reverse", "--check", "--whitespace=nowarn", patch.Path); err != nil {
		// `git apply --check` exits non-zero for "does not apply", which is an
		// answer rather than a fault; a missing executable or unreadable patch
		// surfaces as a start failure instead.
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// verifyDefectConfined proves the working tree differs from HEAD only in the
// files the patch names. Without it a checkout whose Git configuration rewrote
// every line ending would still digest stably and would be measured as if it
// were the authored defect.
func verifyDefectConfined(ctx context.Context, root string, patch DefectPatchFile) error {
	status, err := defectGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	changed := statusPaths(status)
	if len(changed) == 0 {
		return fmt.Errorf("defect patch %s changed nothing in %s", patch.Path, root)
	}
	foreign := make([]string, 0, len(changed))
	for _, path := range changed {
		if _, expected := patch.Paths[path]; !expected {
			foreign = append(foreign, path)
		}
	}
	if len(foreign) != 0 {
		sort.Strings(foreign)
		return fmt.Errorf("benchmark repository reports changes outside defect patch %s: %s", patch.Path, strings.Join(foreign, ", "))
	}
	return nil
}

// statusPaths reads repository-relative paths out of porcelain v1 output. A
// rename record carries both sides; the destination is the one that exists in
// the working tree, so it is the one compared against the patch.
func statusPaths(status string) []string {
	paths := make([]string, 0, 8)
	for _, line := range strings.Split(normalizeLineEndings(status), "\n") {
		if len(line) < 4 {
			continue
		}
		value := strings.TrimSpace(line[3:])
		if arrow := strings.LastIndex(value, " -> "); arrow >= 0 {
			value = value[arrow+4:]
		}
		paths = append(paths, strings.Trim(value, `"`))
	}
	return paths
}

func verifyHead(ctx context.Context, root, expectedCommit string) error {
	head, err := defectGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(head), strings.TrimSpace(expectedCommit)) {
		return fmt.Errorf("repository HEAD is %s, want %s", strings.TrimSpace(head), expectedCommit)
	}
	return nil
}

func normalizeLineEndings(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

// defectGit reads and writes the benchmark checkout. Only path quoting is
// overridden, so that a non-ASCII path can be compared against the patch's own
// file list; end-of-line configuration is deliberately left alone, because
// forcing it would make every file of a checkout written with autocrlf look
// modified and turn a pristine workspace into a false mutation report.
func defectGit(ctx context.Context, root string, args ...string) (string, error) {
	full := append([]string{"-c", "core.quotepath=false"}, args...)
	command := exec.CommandContext(ctx, "git", full...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
	return string(output), nil
}

// DefectWorkspaceState records which authored defect the shared benchmark
// checkout currently carries. The harness has to know this: a patched tree is
// dirty, and Materialize deliberately refuses to switch a dirty checkout, so
// without a record of what it applied the harness could only get past its own
// guard by blindly discarding whatever it found.
type DefectWorkspaceState struct {
	SchemaVersion int    `json:"schema_version"`
	InstanceID    string `json:"instance_id"`
	BaseCommit    string `json:"base_commit"`
	DefectPatch   string `json:"defect_patch"`
	Digest        string `json:"defect_digest"`
	AppliedAt     string `json:"applied_at"`
}

const defectWorkspaceStateName = "workspace-defect.json"

// readDefectWorkspaceState returns the recorded state, or false when the
// workspace is not known to carry a defect.
func readDefectWorkspaceState(outputRoot string) (DefectWorkspaceState, bool, error) {
	body, err := os.ReadFile(filepath.Join(outputRoot, defectWorkspaceStateName))
	if os.IsNotExist(err) {
		return DefectWorkspaceState{}, false, nil
	}
	if err != nil {
		return DefectWorkspaceState{}, false, err
	}
	var state DefectWorkspaceState
	if err := json.Unmarshal(body, &state); err != nil {
		return DefectWorkspaceState{}, false, fmt.Errorf("decode recorded workspace defect state: %w", err)
	}
	if state.SchemaVersion != CampaignSchemaVersion || state.Digest == "" || state.DefectPatch == "" {
		return DefectWorkspaceState{}, false, errors.New("recorded workspace defect state is incomplete")
	}
	return state, true, nil
}

// writeDefectWorkspaceState replaces the record in place. It is deliberately
// not written through WriteJSONAtomic: this file tracks the live workspace and
// changes with every instance, whereas that helper exists to make published
// measurement evidence unreplaceable.
func writeDefectWorkspaceState(outputRoot string, state DefectWorkspaceState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return err
	}
	path := filepath.Join(outputRoot, defectWorkspaceStateName)
	temporary, err := os.CreateTemp(outputRoot, ".workspace-defect-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(append(body, '\n')); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
}

func clearDefectWorkspaceState(outputRoot string) error {
	err := os.Remove(filepath.Join(outputRoot, defectWorkspaceStateName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RevertDefect returns a workspace carrying a recorded defect to the pristine
// checkout the rest of the harness expects. It reverses exactly the patch it
// recorded rather than resetting the tree, so a workspace someone else has
// touched fails loudly instead of having their work deleted.
func RevertDefect(ctx context.Context, root string, state DefectWorkspaceState) error {
	if err := VerifyDefectState(ctx, root, state.BaseCommit, state.Digest); err != nil {
		return fmt.Errorf("workspace no longer matches the recorded defect for %q; refusing to revert it: %w", state.InstanceID, err)
	}
	if _, err := defectGit(ctx, root, "apply", "--reverse", "--whitespace=nowarn", state.DefectPatch); err != nil {
		return fmt.Errorf("revert defect patch %s: %w", state.DefectPatch, err)
	}
	status, err := defectGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("reverting defect patch %s left the benchmark repository dirty: %s", state.DefectPatch, strings.TrimSpace(status))
	}
	return nil
}

func newDefectWorkspaceState(instance Instance, digest string) DefectWorkspaceState {
	return DefectWorkspaceState{
		SchemaVersion: CampaignSchemaVersion, InstanceID: instance.InstanceID,
		BaseCommit: instance.BaseCommit, DefectPatch: instance.DefectPatch,
		Digest: digest, AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}
