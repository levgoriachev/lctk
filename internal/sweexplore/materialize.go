package sweexplore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var safeGitHubRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// remoteSourceURL and scpLikeSource recognize the two shapes Git accepts as a
// remote. Anything else in an override is treated as a local directory, which
// is then proved to be a repository before Git is invoked.
var (
	remoteSourceURL = regexp.MustCompile(`^(?:https|http|ssh|git|file)://`)
	scpLikeSource   = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)
)

// Materialize switches a clean dedicated benchmark checkout to the declared
// base commit. If the object belongs to another benchmark repository, it
// fetches exactly that commit from the repository's configured source before
// switching. It never deletes files, resets changes, or touches a dirty tree.
//
// sourceOverrides maps a benchmark repository name to a local Git directory or
// an explicit remote URL. It exists so a campaign can measure against a private
// repository the models have never seen; a repository without an override keeps
// the strict public GitHub path, so dataset content still cannot inject a
// protocol, host, or filesystem path.
func Materialize(ctx context.Context, root, repository, baseCommit string, sourceOverrides map[string]string) error {
	if strings.TrimSpace(baseCommit) == "" {
		return errors.New("base commit is required")
	}
	status, err := commandOutput(ctx, root, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("benchmark repository is dirty; refusing to change its checkout")
	}
	if _, err := commandOutput(ctx, root, "git", "cat-file", "-e", baseCommit+"^{commit}"); err != nil {
		source, sourceErr := materializeSource(repository, sourceOverrides)
		if sourceErr != nil {
			return sourceErr
		}
		command := exec.CommandContext(ctx, "git", "fetch", "--no-tags", "--depth=1", source, baseCommit)
		command.Dir = root
		output, fetchErr := command.CombinedOutput()
		if fetchErr != nil {
			return fmt.Errorf("fetch base commit %s from %s: %s: %w", baseCommit, repository, strings.TrimSpace(string(output)), fetchErr)
		}
	}
	command := exec.CommandContext(ctx, "git", "switch", "--detach", baseCommit)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("materialize base commit: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return VerifyRepository(ctx, root, baseCommit)
}

// materializeSource resolves where one repository's objects come from. An
// override is owner-declared configuration and may name a path; a repository
// without one is dataset content and stays confined to public GitHub.
func materializeSource(repository string, sourceOverrides map[string]string) (string, error) {
	source, overridden := sourceOverrides[repository]
	if !overridden {
		return publicGitHubURL(repository)
	}
	return ValidateRepositorySource(repository, source)
}

// ValidateRepositorySource proves an override is usable before a campaign
// spends time on it. A local path must actually be a Git repository, so a typo
// fails at configuration time instead of as an opaque fetch error.
func ValidateRepositorySource(repository, source string) (string, error) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" || strings.HasPrefix(trimmed, "-") || strings.ContainsAny(trimmed, "\n\r") {
		return "", fmt.Errorf("repository %q source override is empty or unusable as a Git source", repository)
	}
	if IsRemoteRepositorySource(trimmed) {
		return trimmed, nil
	}
	info, err := os.Stat(trimmed)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("repository %q source override %q is not an existing directory", repository, trimmed)
	}
	if !isGitRepositoryDirectory(trimmed) {
		return "", fmt.Errorf("repository %q source override %q is not a Git repository", repository, trimmed)
	}
	return trimmed, nil
}

// IsRemoteRepositorySource reports whether an override names a remote rather
// than a local directory, which decides whether it is resolved against the
// configuration directory.
func IsRemoteRepositorySource(source string) bool {
	return remoteSourceURL.MatchString(source) || scpLikeSource.MatchString(source)
}

func isGitRepositoryDirectory(directory string) bool {
	// A worktree carries a .git file rather than a directory, so existence is
	// the right test for both.
	if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
		return true
	}
	// A bare repository keeps its object database at the root instead.
	for _, entry := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(directory, entry)); err != nil {
			return false
		}
	}
	return true
}

// publicGitHubURL accepts only the exact owner/repository shape present in the
// pinned SWE-bench source, so dataset content cannot inject a protocol or path.
func publicGitHubURL(repository string) (string, error) {
	if !safeGitHubRepository.MatchString(repository) {
		return "", fmt.Errorf("repository %q is not a safe GitHub owner/repository name", repository)
	}
	return "https://github.com/" + repository + ".git", nil
}
