package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/gitinfo"
)

// Typed codes the Git tools report. They are the gateway's vocabulary, chosen so
// a caller can tell "there is nothing to answer" from "the answer failed".
// A folder that is not a repository is not among them: that is reported as
// an answer, because it is one.
const (
	CodeGitUnavailable = "GIT_UNAVAILABLE"
	CodeGitFailed      = "GIT_FAILED"
	CodeInvalidPath    = "INVALID_PATH"
)

type gitStatusInput struct {
	IncludeUntracked *bool `json:"include_untracked,omitempty"`
	Limit            int   `json:"limit,omitempty"`
}

type gitDiffInput struct {
	Staged   bool     `json:"staged,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Context  int      `json:"context,omitempty"`
	MaxBytes int      `json:"max_bytes,omitempty"`
}

// registerGitTools adds the working-tree tools to a project's server.
//
// They exist because an agent talking to LCTK over HTTP has no shell on the
// user's machine. That is the value LCTK adds here rather than duplicating: an
// editor's own terminal can already run git, and this is for the caller that
// cannot. The scope is the route's, so it can only ever be this project.
func (g *Gateway) registerGitTools(server *mcp.Server, resolved serveContext) {
	if g.options.Git == nil {
		return
	}

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "git_status",
		Description: "What changed since the last commit: branch, commit, and changed files in " +
			"git status --porcelain form. Read-only.",
		InputSchema: schemaObject(nil, map[string]*jsonschema.Schema{
			"include_untracked": schemaBool("List files Git does not track. Defaults to true."),
			"limit":             schemaInt("Maximum changed paths to return. Defaults to 500."),
		}),
	}, func(ctx context.Context, input gitStatusInput, timer *toolTimer) (string, error) {
		untracked := true
		if input.IncludeUntracked != nil {
			untracked = *input.IncludeUntracked
		}
		status, err := backendTimed(g, timer, func() (gitinfo.Status, error) {
			return g.options.Git.Status(ctx, resolved.project.Path, gitinfo.Options{
				MaxFiles:         input.Limit,
				IncludeUntracked: untracked,
			})
		})
		if err != nil {
			return "", asGitToolError(err)
		}
		if !status.Repository {
			return "not a git repository", nil
		}

		var lines []string
		head := "branch " + status.Branch
		switch {
		case status.Unborn:
			head = "repository without a commit yet"
		case status.Detached:
			head = "detached HEAD"
		}
		if status.ShortCommit != "" {
			head += " @ " + status.ShortCommit
		}
		if status.Upstream != "" {
			head += " (upstream " + status.Upstream
			if status.Ahead > 0 {
				head += fmt.Sprintf(", ahead %d", status.Ahead)
			}
			if status.Behind > 0 {
				head += fmt.Sprintf(", behind %d", status.Behind)
			}
			head += ")"
		}
		lines = append(lines, head)
		if status.Prefix != "" {
			lines = append(lines, "project sits at "+status.Prefix+" inside the repository; paths below are repository-relative")
		}
		if !status.Dirty {
			lines = append(lines, "clean")
			return strings.Join(lines, "\n"), nil
		}
		for _, change := range status.Changed {
			marker := stateMarker(change)
			entry := marker + " " + change.Path
			if change.From != "" {
				entry = marker + " " + change.From + " -> " + change.Path
			}
			lines = append(lines, entry)
		}
		if status.Truncated {
			lines = append(lines, fmt.Sprintf("[truncated: %d changed paths total]", status.Total))
		}
		return strings.Join(lines, "\n"), nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "git_diff",
		Description: "Unified diff of the project's uncommitted changes. Read-only and bounded; " +
			"the answer says when it was truncated.",
		InputSchema: schemaObject(nil, map[string]*jsonschema.Schema{
			"staged":    schemaBool("Diff the index against the last commit instead of the working tree."),
			"paths":     schemaStrings("Repository-relative paths to restrict the diff to."),
			"context":   schemaInt("Context lines around each change. Defaults to Git's own."),
			"max_bytes": schemaInt("Maximum patch size. Defaults to 524288, which is also the ceiling."),
		}),
	}, func(ctx context.Context, input gitDiffInput, timer *toolTimer) (string, error) {
		diff, err := backendTimed(g, timer, func() (gitinfo.Diff, error) {
			return g.options.Git.Diff(ctx, resolved.project.Path, gitinfo.DiffOptions{
				Staged:   input.Staged,
				Paths:    input.Paths,
				Context:  input.Context,
				MaxBytes: input.MaxBytes,
			})
		})
		if err != nil {
			return "", asGitToolError(err)
		}
		if !diff.Repository {
			return "not a git repository", nil
		}
		patch := strings.TrimSuffix(diff.Patch, "\n")
		if patch == "" {
			if diff.Staged {
				return "no staged changes", nil
			}
			return "no uncommitted changes", nil
		}
		if diff.Truncated {
			patch += "\n[truncated — narrow with paths or raise max_bytes]"
		}
		return patch, nil
	})
}

// stateMarker maps a change to its porcelain-style marker, which is the form
// every agent already reads fluently.
func stateMarker(change gitinfo.Change) string {
	code := map[string]string{
		"added":      "A",
		"modified":   "M",
		"deleted":    "D",
		"renamed":    "R",
		"copied":     "C",
		"untracked":  "?",
		"conflicted": "U",
	}[change.State]
	if code == "" {
		code = "?"
	}
	if change.State == "untracked" {
		return "??"
	}
	staged := " "
	if change.Staged {
		staged = code
	}
	working := " "
	if change.WorkingTree {
		working = code
	}
	return staged + working
}

// sourceOf reads the working tree for project_info, best effort.
//
// A failure here is not a failure of project_info. A machine without Git, or a
// folder that is not a repository, still has a project worth describing, and the
// absent block says so more clearly than an error would.
func (g *Gateway) sourceOf(ctx context.Context, resolved serveContext) *sourceInfo {
	if g.options.Git == nil {
		return nil
	}
	// The changed list is not wanted here, only its size, so the smallest limit
	// that still counts everything is used.
	status, err := g.options.Git.Status(ctx, resolved.project.Path, gitinfo.Options{
		MaxFiles:         1,
		IncludeUntracked: true,
	})
	if err != nil || !status.Repository {
		return nil
	}
	return &sourceInfo{
		Branch:       status.Branch,
		Detached:     status.Detached,
		Commit:       status.Commit,
		ShortCommit:  status.ShortCommit,
		Upstream:     status.Upstream,
		Ahead:        status.Ahead,
		Behind:       status.Behind,
		Dirty:        status.Dirty,
		ChangedFiles: status.Total,
		Unborn:       status.Unborn,
		Prefix:       status.Prefix,
	}
}

// asGitToolError turns a failure into something a model can act on.
func asGitToolError(err error) error {
	switch {
	case errors.Is(err, gitinfo.ErrGitUnavailable):
		return &searchToolError{
			code:    CodeGitUnavailable,
			message: "Git is not available on this machine.",
			action:  "Install Git, or use exact_search, which does not need it.",
		}
	case isInvalidPath(err):
		return &searchToolError{
			code:    CodeInvalidPath,
			message: err.Error(),
			action:  "Use a repository-relative path.",
		}
	default:
		return &searchToolError{
			code:      CodeGitFailed,
			message:   "Git could not answer: " + firstLine(err.Error()),
			retryable: true,
			action:    "Check the repository with git status in a terminal.",
		}
	}
}

// isInvalidPath recognizes the refusals cleanPaths produces, which are the
// caller's fault rather than the repository's.
func isInvalidPath(err error) bool {
	message := err.Error()
	return strings.Contains(message, "repository-relative") ||
		strings.Contains(message, "stay inside the repository") ||
		strings.Contains(message, "must not begin with a dash") ||
		strings.Contains(message, "a path is empty")
}

// firstLine keeps a tool error to one line. Git's diagnostics can run to several,
// and the first is the one that says what happened.
func firstLine(message string) string {
	if index := strings.IndexAny(message, "\r\n"); index >= 0 {
		return strings.TrimSpace(message[:index])
	}
	return strings.TrimSpace(message)
}
