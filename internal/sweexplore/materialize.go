package sweexplore

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var safeGitHubRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Materialize switches a clean dedicated benchmark checkout to the declared
// base commit. If the object belongs to another benchmark repository, it
// fetches exactly that public GitHub commit before switching. It never deletes
// files, resets changes, or touches a dirty tree.
func Materialize(ctx context.Context, root, repository, baseCommit string) error {
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
		url, urlErr := publicGitHubURL(repository)
		if urlErr != nil {
			return urlErr
		}
		command := exec.CommandContext(ctx, "git", "fetch", "--no-tags", "--depth=1", url, baseCommit)
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

// publicGitHubURL accepts only the exact owner/repository shape present in the
// pinned SWE-bench source, so dataset content cannot inject a protocol or path.
func publicGitHubURL(repository string) (string, error) {
	if !safeGitHubRepository.MatchString(repository) {
		return "", fmt.Errorf("repository %q is not a safe GitHub owner/repository name", repository)
	}
	return "https://github.com/" + repository + ".git", nil
}
