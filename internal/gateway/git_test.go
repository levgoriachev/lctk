package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/gitinfo"
)

// fakeGit records which root it was asked about, which is how the scope
// assertions below can tell that the route decided and the arguments did not.
type fakeGit struct {
	roots  []string
	status gitinfo.Status
	diff   gitinfo.Diff
	err    error
}

func (f *fakeGit) Status(_ context.Context, root string, _ gitinfo.Options) (gitinfo.Status, error) {
	f.roots = append(f.roots, root)
	return f.status, f.err
}

func (f *fakeGit) Diff(_ context.Context, root string, options gitinfo.DiffOptions) (gitinfo.Diff, error) {
	f.roots = append(f.roots, root)
	if f.err != nil {
		return gitinfo.Diff{}, f.err
	}
	diff := f.diff
	diff.Staged = options.Staged
	return diff, nil
}

func gitFixture(t *testing.T, git *fakeGit) *fixture {
	t.Helper()
	f := newFixture(t, true, "alpha-aaaaaaaa")
	f.git = git
	return f
}

// TestGitStatusReportsTheWorkingTree pins the porcelain-shaped answer: a head
// line with the upstream position, then one marker line per change.
func TestGitStatusReportsTheWorkingTree(t *testing.T) {
	git := &fakeGit{status: gitinfo.Status{
		Repository: true, Branch: "main", Commit: "abc123", ShortCommit: "abc123",
		Dirty: true, Total: 2, Upstream: "origin/main", Ahead: 1,
		Changed: []gitinfo.Change{
			{Path: "a.go", State: "modified", WorkingTree: true},
			{Path: "b.go", State: "untracked", WorkingTree: true},
		},
	}}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, result := callText(t, session, "git_status", nil)
	if result.IsError {
		t.Fatalf("git_status refused: %s", resultText(result))
	}
	want := "branch main @ abc123 (upstream origin/main, ahead 1)\n M a.go\n?? b.go"
	if text != want {
		t.Errorf("answer = %q, want %q", text, want)
	}
}

func TestGitStatusSaysCleanAndSaysTruncated(t *testing.T) {
	git := &fakeGit{status: gitinfo.Status{Repository: true, Branch: "main", ShortCommit: "abc123"}}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, _ := callText(t, session, "git_status", nil)
	if text != "branch main @ abc123\nclean" {
		t.Errorf("clean answer = %q", text)
	}

	git.status = gitinfo.Status{
		Repository: true, Branch: "main", ShortCommit: "abc123",
		Dirty: true, Total: 7, Truncated: true,
		Changed: []gitinfo.Change{{Path: "a.go", State: "modified", WorkingTree: true}},
	}
	text, _ = callText(t, session, "git_status", nil)
	if !strings.HasSuffix(text, "[truncated: 7 changed paths total]") {
		t.Errorf("a truncated status does not say so:\n%s", text)
	}
}

// The same guarantee the other tools carry: a model-supplied project or path
// does not decide what is read.
func TestGitStatusIgnoresScopeLikeArguments(t *testing.T) {
	git := &fakeGit{status: gitinfo.Status{Repository: true, Branch: "main"}}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	_, result := callText(t, session, "git_status", map[string]any{
		"project_id":      "beta-bbbbbbbb",
		"repository_root": "/etc",
		"path":            "../../elsewhere",
	})
	if result.IsError {
		t.Fatalf("scope-like arguments caused a refusal: %s", resultText(result))
	}
	for _, root := range git.roots {
		if root != "/work/alpha-aaaaaaaa" {
			t.Fatalf("git was asked about %q, which is not the routed project's path", root)
		}
	}
}

// A folder that is not a repository is an answer, not a failure. Plenty of
// projects are just folders.
func TestAProjectThatIsNotARepositoryAnswersRatherThanFails(t *testing.T) {
	f := gitFixture(t, &fakeGit{status: gitinfo.Status{Repository: false}})
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, result := callText(t, session, "git_status", nil)
	if result.IsError {
		t.Fatalf("a plain folder produced a tool error: %s", resultText(result))
	}
	if text != "not a git repository" {
		t.Errorf("answer = %q", text)
	}
}

func TestGitDiffReturnsThePatchAndHonoursStaged(t *testing.T) {
	git := &fakeGit{diff: gitinfo.Diff{Repository: true, Patch: "diff --git a/a.go b/a.go\n"}}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, result := callText(t, session, "git_diff", nil)
	if result.IsError {
		t.Fatalf("git_diff refused: %s", resultText(result))
	}
	if text != "diff --git a/a.go b/a.go" {
		t.Errorf("patch = %q", text)
	}

	// An empty staged diff answers in staged terms, which proves the flag
	// reached the reader.
	git.diff = gitinfo.Diff{Repository: true}
	text, _ = callText(t, session, "git_diff", map[string]any{"staged": true})
	if text != "no staged changes" {
		t.Errorf("staged answer = %q", text)
	}
	text, _ = callText(t, session, "git_diff", nil)
	if text != "no uncommitted changes" {
		t.Errorf("working-tree answer = %q", text)
	}
}

func TestGitDiffSaysWhenThePatchWasTruncated(t *testing.T) {
	git := &fakeGit{diff: gitinfo.Diff{Repository: true, Patch: "diff --git a/a.go b/a.go\n", Truncated: true}}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, _ := callText(t, session, "git_diff", nil)
	if !strings.HasSuffix(text, "[truncated — narrow with paths or raise max_bytes]") {
		t.Errorf("a truncated patch does not say so:\n%s", text)
	}
}

// A path is how a caller would try to read outside the project. The refusal has
// to reach the model as something it can act on.
func TestGitDiffRefusesAnEscapingPathWithAnActionableError(t *testing.T) {
	git := &fakeGit{err: errors.New(`the path must stay inside the repository: "../secrets"`)}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	_, result := callText(t, session, "git_diff", map[string]any{
		"paths": []string{"../secrets"},
	})
	if result == nil || !result.IsError {
		t.Fatal("an escaping path was accepted")
	}
	message := resultText(result)
	if !strings.Contains(message, CodeInvalidPath) {
		t.Fatalf("error = %q, want the typed code %s", message, CodeInvalidPath)
	}
}

// A machine without Git should say so plainly and point at what still works,
// rather than producing a failure the model has to guess at.
func TestAMachineWithoutGitSaysSo(t *testing.T) {
	f := gitFixture(t, &fakeGit{err: gitinfo.ErrGitUnavailable})
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	_, result := callText(t, session, "git_status", nil)
	if result == nil || !result.IsError {
		t.Fatal("a missing git produced no error")
	}
	message := resultText(result)
	if !strings.Contains(message, CodeGitUnavailable) || !strings.Contains(message, "exact_search") {
		t.Fatalf("error = %q, want the typed code and what still works", message)
	}
}

// project_info carries the commit-and-dirty half of the freshness contract. It
// is best effort: a project that is not a repository still has a description.
func TestProjectInfoCarriesTheSourceState(t *testing.T) {
	git := &fakeGit{status: gitinfo.Status{
		Repository: true, Branch: "main", Commit: "abc1234def", ShortCommit: "abc1234",
		Dirty: true, Total: 3, Upstream: "origin/main", Ahead: 1,
	}}
	f := gitFixture(t, git)
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text := callProjectInfo(t, session, nil)
	if !strings.Contains(text, "source: branch main @ abc1234, 3 changed files") {
		t.Errorf("the source state did not reach the caller:\n%s", text)
	}
	capabilities := capabilitiesLine(t, text)
	for _, want := range []string{"git_status", "git_diff"} {
		if !strings.Contains(capabilities, want) {
			t.Errorf("capabilities = %q, want %s listed", capabilities, want)
		}
	}
}

func TestProjectInfoOmitsSourceWhenGitCannotAnswer(t *testing.T) {
	f := gitFixture(t, &fakeGit{err: gitinfo.ErrGitUnavailable})
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text := callProjectInfo(t, session, nil)
	if strings.Contains(text, "source:") {
		t.Errorf("a source block was reported when Git cannot answer:\n%s", text)
	}
	// The rest of the answer still has to be there.
	if !strings.Contains(text, "project: alpha-aaaaaaaa") {
		t.Errorf("project_info stopped answering:\n%s", text)
	}
	if strings.Contains(capabilitiesLine(t, text), "git_status") {
		t.Errorf("git tools were advertised on a machine without Git:\n%s", text)
	}
}

// A gateway with no Git reader advertises no Git tools, rather than advertising
// ones that would always fail. The tool list is a contract a model plans
// against, so a tool that cannot work should not be in it.
func TestAGatewayWithoutGitAdvertisesNoGitTools(t *testing.T) {
	// Connected in memory rather than through the fixture, because the fixture
	// always installs a reader and the question here is what happens without one.
	server := New(Options{}).newProjectServer(serveContext{})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(t.Context(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range tools.Tools {
		registered[tool.Name] = true
	}
	for _, name := range []string{"git_status", "git_diff", "run_command"} {
		if registered[name] {
			t.Errorf("a gateway with no reader or runner registered %s", name)
		}
	}
	for _, name := range []string{"project_info", "read_span", "exact_search"} {
		if !registered[name] {
			t.Errorf("the gateway stopped serving %s", name)
		}
	}
}
