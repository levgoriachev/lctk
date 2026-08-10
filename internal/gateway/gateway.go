package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/gitinfo"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/projectauth"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

// RegistryLoader returns the current registrations. It is a function rather than
// a snapshot so a project registered while the daemon runs becomes reachable
// without a restart.
type RegistryLoader func() (*projectregistry.Registry, error)

// AuthorizationLoader returns the daemon-owned OAuth authority.
type AuthorizationLoader func() (*projectauth.Store, error)

// StatusProbe reports the runtime state of a project.
type StatusProbe func(ctx context.Context, project projectregistry.Project) (projectstack.Status, error)

// Waker tells the host that a client is using a project.
//
// It is how "on-demand wakeup" reaches the watcher: a project nobody has talked
// to for a while releases its filesystem watches, and the request that brings it
// back into use is the right moment to take them again. It must not block, since
// it runs on the request path.
type Waker func(project projectregistry.Project, status projectstack.Status)

// ChangeState is what the host watcher knows about a project right now.
type ChangeState struct {
	Watching bool
	Pending  int
	// Indexing says the host is bringing the index up to date right now, which is
	// worth waiting for in a way that "behind and idle" is not.
	Indexing bool
	// GapReason is set when the host's record of changes is known to be
	// incomplete, in which case the pending count is a lower bound.
	GapReason       string
	LastEventAt     time.Time
	DebounceSeconds float64
}

// ChangeReporter returns the watcher's view of a project, and false when no
// watcher is running for it.
type ChangeReporter func(projectID string) (ChangeState, bool)

// Flusher brings a project's index up to date now rather than at the end of its
// debounce window, returning when the work is done or the context expires.
type Flusher func(ctx context.Context, projectID string)

// searchFlushBudget bounds how long a search waits for pending changes to reach
// the index.
//
// A caller that has just written a file and immediately searches wants to see it,
// and a second or two of waiting is a far better answer than a confidently wrong
// one. The bound exists because the wait cannot be unlimited: past it the search
// runs against what the index already holds and says so, which is still honest.
const searchFlushBudget = 5 * time.Second

// Options configures a gateway.
type Options struct {
	Registry       RegistryLoader
	Authorizations AuthorizationLoader
	Status         StatusProbe
	Logger         *slog.Logger
	// Now is injectable so expiry behavior is testable.
	Now func() time.Time
	// RequireRunning gates tool calls on the project's stack being healthy. It is
	// disabled in tests that exercise routing and scope without containers.
	RequireRunning bool
	// NewSearchClient builds the adapter to a project's code-intelligence
	// service. It is injectable so search behavior can be tested against a
	// stand-in service without a container.
	NewSearchClient func(address string) *codeintel.Client
	// Wake and Changes connect the route to the host change watcher. Both are
	// optional: a gateway without them serves every tool, and reports that
	// freshness is unknown rather than implying it is current.
	Wake    Waker
	Changes ChangeReporter
	// Flush lets a search wait for observed changes to reach the index instead of
	// answering from a generation it already knows is behind.
	Flush Flusher
	// Runner executes the project's approved commands, Manifest reads what the
	// repository proposes, Budget says what a command may cost, and Audit records
	// what ran. All are optional: without a Runner the project serves no
	// run_command tool at all, which is better than serving one that must fail.
	Runner   CommandRunner
	Manifest ManifestLoader
	Budget   func(project projectregistry.Project) hostsettings.Budget
	Audit    Auditor
	// Git reads the project's working tree through Git. It is optional: a build
	// without it serves every other tool and reports that the project's source
	// state is unknown, which is what a machine without git would produce anyway.
	Git GitReader
}

// GitReader is the part of internal/gitinfo the gateway drives.
type GitReader interface {
	Status(ctx context.Context, root string, options gitinfo.Options) (gitinfo.Status, error)
	Diff(ctx context.Context, root string, options gitinfo.DiffOptions) (gitinfo.Diff, error)
}

// Gateway serves /projects/{project_id}/mcp.
type Gateway struct {
	options Options
}

// New builds a gateway, filling in defaults.
func New(options Options) *Gateway {
	if options.Registry == nil {
		options.Registry = projectregistry.Load
	}
	if options.Authorizations == nil {
		options.Authorizations = projectauth.Open
	}
	if options.Status == nil {
		manager := projectstack.NewManager()
		options.Status = manager.Status
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Gateway{options: options}
}

// Register attaches the project route to a mux.
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.Handle("/projects/{project_id}/mcp", g)
	mux.Handle("/projects/{project_id}/mcp/", g)
}

// projectInfoInput accepts the scope-like arguments a model may invent, purely so
// they can be visibly ignored. They are declared, and documented as ignored, to
// make the contract explicit rather than leaving a model to guess.
type projectInfoInput struct {
	ProjectID      string `json:"project_id,omitempty" jsonschema:"Ignored. The authoritative project comes from the endpoint."`
	RepositoryRoot string `json:"repository_root,omitempty" jsonschema:"Ignored. The authoritative root comes from the registry."`
	Path           string `json:"path,omitempty" jsonschema:"Ignored. The authoritative root comes from the registry."`
}

// projectInfoOutput is what a calling agent needs to orient itself in a project.
type projectInfoOutput struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Profile   string `json:"profile"`
	// Root is the path as the project's own tools see it, not the host path. The
	// host path is deliberately not exposed to the client.
	Root string `json:"root"`
	// ScopeSource states where the answer came from, so a caller can verify that
	// its own arguments did not influence it.
	ScopeSource string `json:"scope_source"`
	State       string `json:"state"`
	Health      string `json:"health,omitempty"`
	Retryable   bool   `json:"retryable"`
	// Capabilities names the tools this project can actually serve right now, so
	// a caller does not have to discover by failure that search is unavailable.
	Capabilities []string `json:"capabilities"`
	ServerTime   string   `json:"server_time"`
	Version      string   `json:"version"`
	// Index describes the project's search index when there is one.
	Index *indexInfo `json:"index,omitempty"`
	// Changes describes what the host watcher has seen since the index was last
	// brought up to date.
	Changes *changeInfo `json:"changes,omitempty"`
	// Source is what Git says about the working tree, absent when the project is
	// not a repository or Git is unavailable.
	Source *sourceInfo `json:"source,omitempty"`
	// Commands names what run_command can actually run right now, so a caller
	// does not have to discover by refusal that nothing has been approved.
	Commands []string `json:"commands,omitempty"`
	// OutlineLanguages names what file_outline can describe, so a caller learns
	// the boundary by asking rather than by being refused on a file.
	OutlineLanguages []string `json:"outline_languages,omitempty"`
}

// sourceInfo is the commit-and-dirty half of the freshness contract.
type sourceInfo struct {
	Branch      string `json:"branch,omitempty"`
	Detached    bool   `json:"detached,omitempty"`
	Commit      string `json:"commit,omitempty"`
	ShortCommit string `json:"short_commit,omitempty"`
	Upstream    string `json:"upstream,omitempty"`
	Ahead       int    `json:"ahead,omitempty"`
	Behind      int    `json:"behind,omitempty"`
	Dirty       bool   `json:"dirty"`
	// ChangedFiles is how many paths differ from the commit, so a caller learns
	// the size of the difference without asking for the list.
	ChangedFiles int `json:"changed_files"`
	// Unborn marks a repository with no commit yet.
	Unborn bool `json:"unborn,omitempty"`
	// Prefix is where the project sits inside the repository, so a caller can
	// relate the repository-relative paths git_status returns to the project.
	Prefix string `json:"prefix,omitempty"`
}

// indexInfo is the freshness ADR-0004 requires a project answer to carry.
type indexInfo struct {
	Ready      bool   `json:"ready"`
	Indexing   bool   `json:"indexing"`
	Generation uint64 `json:"generation"`
	FileCount  int    `json:"file_count"`
	IndexedAt  string `json:"indexed_at,omitempty"`
	// Freshness is one of fresh, updating, stale, or unknown. It is never
	// optimistic: a project whose changes nobody is watching reports unknown
	// rather than fresh, because "nothing was observed" is not evidence that
	// nothing changed.
	Freshness string `json:"freshness"`
	Reason    string `json:"reason,omitempty"`
}

// changeInfo is the host watcher's view, reported so a caller can judge a search
// result rather than assume it.
type changeInfo struct {
	// Watching says a native watcher is registered for this project right now.
	Watching bool `json:"watching"`
	// Pending counts files changed since the index was last brought up to date.
	Pending int `json:"pending"`
	// Indexing says those changes are being applied right now, which tells a
	// caller that retrying shortly is worth more than retrying eventually.
	Indexing bool `json:"indexing,omitempty"`
	// Complete says the record of those changes is known to be whole. When it is
	// false, Pending is a lower bound and GapReason says why.
	Complete    bool   `json:"complete"`
	GapReason   string `json:"gap_reason,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
	// DebounceSeconds is how long after the last save an update is expected, so a
	// caller that just wrote a file knows whether to wait or to search now.
	DebounceSeconds float64 `json:"debounce_seconds,omitempty"`
}

// Freshness values.
const (
	freshnessFresh    = "fresh"
	freshnessUpdating = "updating"
	freshnessStale    = "stale"
	freshnessUnknown  = "unknown"
)

// describeChanges turns the watcher's view into the reported block and the
// freshness verdict.
//
// The verdict is deliberately pessimistic in every uncertain case. An agent that
// is told "fresh" will not re-check, so claiming freshness without evidence is
// the one answer that causes silent wrong work.
func describeChanges(state ChangeState, watching bool, indexing bool) (*changeInfo, string) {
	// A build in the project service and a drain on the host are both "the index
	// is being brought up to date"; a caller does not care which.
	indexing = indexing || state.Indexing

	freshness := freshnessUnknown
	if indexing {
		freshness = freshnessUpdating
	}
	if !watching {
		return nil, freshness
	}

	info := &changeInfo{
		Watching:        state.Watching,
		Pending:         state.Pending,
		Indexing:        indexing,
		Complete:        state.GapReason == "",
		GapReason:       state.GapReason,
		DebounceSeconds: state.DebounceSeconds,
	}
	if !state.LastEventAt.IsZero() {
		info.LastEventAt = state.LastEventAt.UTC().Format(time.RFC3339)
	}

	switch {
	case indexing:
		freshness = freshnessUpdating
	case !info.Complete:
		// The record is incomplete, so the index may be behind by an unknown
		// amount. Stale is the honest verdict; the reason says why.
		freshness = freshnessStale
	case state.Pending > 0:
		freshness = freshnessStale
	default:
		freshness = freshnessFresh
	}
	return info, freshness
}

// exactSearchInput is the public request schema for exact_search.
//
// The scope-like fields are declared and documented as ignored for the same
// reason project_info declares them: a model will supply them, and an explicit
// "this is ignored" is a better contract than silence.
type exactSearchInput struct {
	Pattern string `json:"pattern" jsonschema:"The text or regular expression to find. Required."`
	Mode    string `json:"mode,omitempty" jsonschema:"literal (default) or regex."`

	CaseSensitive bool     `json:"case_sensitive,omitempty" jsonschema:"Match case exactly. Defaults to false."`
	PathGlobs     []string `json:"path_globs,omitempty" jsonschema:"Project-relative globs such as **/*.go. An absolute or escaping glob is refused."`
	Languages     []string `json:"languages,omitempty" jsonschema:"Restrict to languages such as Go or TypeScript."`
	Limit         int      `json:"limit,omitempty" jsonschema:"Maximum matches per page. Defaults to 50, maximum 500."`
	Cursor        string   `json:"cursor,omitempty" jsonschema:"Continue from a previous response's next_cursor. A cursor is only valid for the index generation that produced it."`

	ProjectID      string `json:"project_id,omitempty" jsonschema:"Ignored. The authoritative project comes from the endpoint."`
	RepositoryRoot string `json:"repository_root,omitempty" jsonschema:"Ignored. The authoritative root comes from the registry."`
	Path           string `json:"path,omitempty" jsonschema:"Ignored. Use path_globs to filter within the project."`
}

// exactSearchOutput is the public response schema.
type exactSearchOutput struct {
	ProjectID string            `json:"project_id"`
	Matches   []codeintel.Match `json:"matches"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated"`
	// NextCursor is present only when more results exist.
	NextCursor string `json:"next_cursor,omitempty"`
	// ScopeSource states where the searched project came from, so a caller can
	// verify that its own arguments did not influence it.
	ScopeSource string               `json:"scope_source"`
	Root        string               `json:"root"`
	Provenance  codeintel.Provenance `json:"provenance"`
	// Changes says what the host has observed but not yet applied. It is present
	// only when it has something to report, which after a flush means the index
	// could not be brought fully up to date.
	Changes *changeInfo `json:"changes,omitempty"`
}

// serveContext is everything resolved for one authenticated request.
type serveContext struct {
	project       projectregistry.Project
	authorization projectauth.Authorization
	status        projectstack.Status
	requestID     string
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	// The route is authoritative. Nothing below reads a project identifier from
	// the request body.
	projectID := r.PathValue("project_id")

	logger := g.options.Logger.With(
		slog.String("request_id", requestID),
		slog.String("project_id", projectID),
		slog.String("method", r.Method),
	)

	resolved, failure := g.resolve(r, projectID, requestID)
	if failure != nil {
		logger.Warn("project request refused",
			slog.String("code", failure.err.Code),
			slog.Bool("retryable", failure.err.Retryable))
		if failure.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, projectauth.MetadataURL(r.Host, projectID), projectauth.ScopeProject))
		}
		writeError(w, failure.status, failure.err)
		return
	}

	logger.Info("project request accepted",
		slog.String("authorization_id", resolved.authorization.ID),
		slog.String("client", resolved.authorization.ClientName),
		slog.String("state", string(resolved.status.State)))

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return g.newProjectServer(*resolved)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	handler.ServeHTTP(w, r)
}

type refusal struct {
	status int
	err    TypedError
}

// resolve performs routing, authentication, and lifecycle gating in the order
// that reveals the least: a caller learns a project exists only after presenting
// a credential that covers it.
func (g *Gateway) resolve(r *http.Request, projectID, requestID string) (*serveContext, *refusal) {
	fail := func(status int, code, message, action string, retryable bool) *refusal {
		return &refusal{status: status, err: TypedError{
			Code:              code,
			Message:           message,
			Retryable:         retryable,
			RecommendedAction: action,
			ProjectID:         projectID,
			RequestID:         requestID,
		}}
	}

	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return nil, fail(http.StatusUnauthorized, CodeAuthRequired,
			"OAuth authorization is required.",
			"Authenticate this Streamable HTTP MCP server in the client, then retry.", false)
	}

	authorizations, err := g.options.Authorizations()
	if err != nil {
		return nil, fail(http.StatusInternalServerError, CodeInternalError,
			"OAuth authorizations could not be read.", "Inspect the LCTK home directory.", true)
	}

	now := g.options.Now()
	authorization, err := authorizations.ResolveAccessToken(token, projectID, projectauth.ResourceURL(r.Host, projectID), now)
	switch {
	case errors.Is(err, projectauth.ErrTokenNotFound):
		return nil, fail(http.StatusUnauthorized, CodeAuthRequired,
			"The presented OAuth access token is not valid.",
			"Authenticate this MCP server again in the client.", false)
	case errors.Is(err, projectauth.ErrTokenExpired):
		return nil, fail(http.StatusUnauthorized, CodeAuthRequired,
			"The OAuth access token has expired.", "Refresh or authenticate this MCP server again in the client.", false)
	case errors.Is(err, projectauth.ErrWrongProject):
		// A real credential scoped to another project. This is the case that
		// keeps one project's key from opening another.
		return nil, fail(http.StatusForbidden, CodeAuthForbidden,
			"The OAuth token was issued for another project resource.",
			"Authenticate against this project's exact MCP URL.", false)
	case err != nil:
		return nil, fail(http.StatusInternalServerError, CodeInternalError,
			"The OAuth access token could not be validated.", "", true)
	}

	registry, err := g.options.Registry()
	if err != nil {
		return nil, fail(http.StatusInternalServerError, CodeRegistryUnavailable,
			"The project registry could not be read.",
			"Inspect the LCTK home directory.", true)
	}

	// Lookup is by exact identifier only. The convenience matching the CLI offers
	// must never establish scope for a request.
	var project projectregistry.Project
	found := false
	for _, candidate := range registry.List() {
		if candidate.ID == projectID {
			project = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, fail(http.StatusNotFound, CodeProjectNotFound,
			"The project is not registered.",
			"Register it with lctk project add.", false)
	}

	status, statusErr := g.options.Status(r.Context(), project)
	if statusErr != nil {
		switch {
		case errors.Is(statusErr, projectstack.ErrLinuxContainersRequired):
			return nil, fail(http.StatusServiceUnavailable, CodeRuntimeUnsuitable,
				"The container runtime cannot run Linux containers.",
				"Repair or reinstall the LCTK managed runtime.", false)
		case errors.Is(statusErr, projectstack.ErrRuntimeUnavailable):
			return nil, fail(http.StatusServiceUnavailable, CodeRuntimeUnavailable,
				"The container runtime is unavailable.",
				"Start or repair the LCTK managed runtime from the Admin UI.", true)
		}
	}

	if g.options.RequireRunning {
		switch status.State {
		case projectstack.StateRunning:
		case projectstack.StateStarting:
			return nil, fail(http.StatusServiceUnavailable, CodeProjectStarting,
				"The project is starting.", "Retry shortly.", true)
		case projectstack.StateStopped:
			return nil, fail(http.StatusServiceUnavailable, CodeProjectStopped,
				"The project is stopped.",
				"Start it with lctk project start.", false)
		case projectstack.StateError:
			return nil, fail(http.StatusServiceUnavailable, CodeServiceUnavailable,
				"The project stack is unhealthy.",
				"Inspect it with lctk project status.", false)
		default:
			return nil, fail(http.StatusServiceUnavailable, CodeServiceUnavailable,
				"The project state could not be determined.",
				"Inspect it with lctk project status.", true)
		}
	}

	// The request is accepted, so the project is in use. Waking the watcher here
	// rather than on a timer is what makes observation follow actual use.
	if g.options.Wake != nil {
		g.options.Wake(project, status)
	}

	return &serveContext{project: project, authorization: authorization, status: status, requestID: requestID}, nil
}

// newProjectServer builds an MCP server bound to one resolved project. The
// closure captures the project, so no tool can reach another one.
func (g *Gateway) newProjectServer(resolved serveContext) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "lctk-project-" + resolved.project.ID,
		Version: buildinfo.Version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "project_info",
		Description: "Return the project this endpoint is bound to. " +
			"The scope comes from the endpoint and the server-side registry; " +
			"arguments naming a project or path are ignored.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ projectInfoInput) (*mcp.CallToolResult, projectInfoOutput, error) {
		output := projectInfoOutput{
			ProjectID:    resolved.project.ID,
			Name:         resolved.project.Name,
			Profile:      string(resolved.project.Profile),
			Root:         projectstack.WorkspaceMount,
			ScopeSource:  "route_and_registry",
			State:        string(resolved.status.State),
			Health:       resolved.status.Health,
			Retryable:    resolved.status.State.Retryable(),
			Capabilities: []string{"project_info"},
			ServerTime:   g.options.Now().UTC().Format(time.RFC3339),
			Version:      buildinfo.Version,
		}
		var state ChangeState
		watching := false
		if g.options.Changes != nil {
			state, watching = g.options.Changes(resolved.project.ID)
		}

		if resolved.status.ServiceAddress != "" {
			output.Capabilities = append(output.Capabilities, "exact_search")
			// The index status is reported best-effort. A project that is up but
			// whose service is still starting should still answer project_info.
			if status, err := g.searchClient(resolved).Status(ctx); err == nil {
				// Advertised from what the service reports rather than from what this
				// build can do: a project whose container predates the symbol layer
				// answers nothing here, and claiming the tool anyway would send a
				// caller to discover the gap by being refused.
				if len(status.OutlineLanguages) > 0 {
					output.Capabilities = append(output.Capabilities,
						"file_outline", "find_definition", "find_references")
					output.OutlineLanguages = status.OutlineLanguages
				}
				if status.Semantic != nil {
					output.Capabilities = append(output.Capabilities, "code_search_semantic")
				}
				if status.Graph != nil {
					output.Capabilities = append(output.Capabilities,
						"callers_find", "callees_find", "dependency_path", "impact_analyze", "repository_map",
						"memory_get", "memory_search", "memory_put", "memory_delete")
				}
				changes, freshness := describeChanges(state, watching, status.Indexing)
				output.Changes = changes
				output.Index = &indexInfo{
					Ready:      status.Ready,
					Indexing:   status.Indexing,
					Generation: status.Generation,
					FileCount:  status.FileCount,
					IndexedAt:  status.IndexedAt,
					Freshness:  freshness,
					Reason:     status.Reason,
				}
			}
		}
		if output.Changes == nil {
			// No index to describe, but the watcher's view still tells a caller
			// whether anything is being observed at all.
			output.Changes, _ = describeChanges(state, watching, false)
		}
		output.Source = g.sourceOf(ctx, resolved)
		if output.Source != nil {
			output.Capabilities = append(output.Capabilities, "git_status", "git_diff")
		}
		if runnable := g.runnableNames(resolved); len(runnable) > 0 {
			output.Capabilities = append(output.Capabilities, "run_command")
			output.Commands = runnable
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "exact_search",
		Description: "Search this project's saved working tree for an exact literal or regular expression. " +
			"Results are indexed and include files that are saved but not committed. " +
			"They describe the files as they are on disk: an edit that has not been written " +
			"to disk is not searchable here, whether it is yours or a proposal another client " +
			"is holding. " +
			"Paths are project-relative; the scope comes from the endpoint, and arguments naming " +
			"a project or an absolute path are ignored or refused.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input exactSearchInput) (*mcp.CallToolResult, exactSearchOutput, error) {
		if resolved.status.ServiceAddress == "" {
			return nil, exactSearchOutput{}, &searchToolError{
				code:    codeintel.CodeSearchUnsupported,
				message: "This project does not expose a code-intelligence service.",
				action:  "Restart it with lctk project restart so it is recreated with the current stack definition.",
			}
		}

		// Bring the index up to date before answering. An agent that just wrote a
		// file and searched for it is the common case, and waiting a moment beats
		// telling it the code it wrote does not exist.
		if g.options.Flush != nil {
			flushCtx, cancel := context.WithTimeout(ctx, searchFlushBudget)
			g.options.Flush(flushCtx, resolved.project.ID)
			cancel()
		}

		response, err := g.searchClient(resolved).Search(ctx, codeintel.Request{
			Pattern:       input.Pattern,
			Mode:          input.Mode,
			CaseSensitive: input.CaseSensitive,
			PathGlobs:     input.PathGlobs,
			Languages:     input.Languages,
			Limit:         input.Limit,
			Cursor:        input.Cursor,
		})
		if err != nil {
			return nil, exactSearchOutput{}, asSearchToolError(err)
		}

		// Freshness is judged after the search, not before: the flush above may
		// have applied everything, in which case there is nothing left to warn
		// about, and reporting a state read earlier would be stale by one step.
		output := exactSearchOutput{
			ProjectID:   resolved.project.ID,
			Matches:     response.Matches,
			Total:       response.Total,
			Truncated:   response.Truncated,
			NextCursor:  response.NextCursor,
			ScopeSource: "route_and_registry",
			Root:        projectstack.WorkspaceMount,
			Provenance:  response.Provenance,
		}
		if g.options.Changes != nil {
			state, watching := g.options.Changes(resolved.project.ID)
			changes, freshness := describeChanges(state, watching, false)
			output.Provenance.Freshness = freshness
			if freshness != freshnessFresh {
				output.Changes = changes
			}
		}
		return nil, output, nil
	})

	g.registerOutlineTool(server, resolved)
	g.registerSymbolTools(server, resolved)
	g.registerSemanticTool(server, resolved)
	g.registerIntelligenceTools(server, resolved)
	g.registerGitTools(server, resolved)
	g.registerRunTool(server, resolved)
	return server
}

// fileOutlineInput is the public request schema for file_outline.
type fileOutlineInput struct {
	Path string `json:"path" jsonschema:"The project-relative path of one file. Required. An absolute or escaping path is refused."`

	ProjectID      string `json:"project_id,omitempty" jsonschema:"Ignored. The authoritative project comes from the endpoint."`
	RepositoryRoot string `json:"repository_root,omitempty" jsonschema:"Ignored. The authoritative root comes from the registry."`
}

// fileOutlineOutput is the public response schema.
type fileOutlineOutput struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Language  string `json:"language"`
	Bytes     int    `json:"bytes"`
	Lines     int    `json:"lines"`
	// Digest is the content this answer describes. It is a digest rather than an
	// index generation because an outline reads the file, so it cannot be behind.
	Digest  string             `json:"digest,omitempty"`
	Symbols []codeintel.Symbol `json:"symbols"`
	Syntax  codeintel.Syntax   `json:"syntax"`
	// ScopeSource states where the answered project came from, so a caller can
	// verify that its own arguments did not influence it.
	ScopeSource string `json:"scope_source"`
	Root        string `json:"root"`
	// Provenance names what produced the answer and how precise it is. Nothing here
	// is type-resolved, and the answer says so rather than leaving a caller to
	// assume otherwise.
	Provenance outlineProvenance `json:"provenance"`
}

// outlineProvenance is the symbol layer's own provenance.
//
// It is a separate shape from the search provenance because the two describe
// different things: a search answer is about an index generation, and an outline is
// about the bytes of one file.
type outlineProvenance struct {
	Backend       string `json:"backend"`
	SchemaVersion int    `json:"schema_version"`
	// Precision is what kind of answer this is. "syntax" means the declarations are
	// what the file's own syntax says, with no types resolved and nothing consulted
	// outside this file.
	Precision string `json:"precision"`
	// ReadAt is when the file was read, which is what "current" means for an answer
	// that does not come from an index.
	ReadAt string `json:"read_at"`
}

// registerOutlineTool adds file_outline when the project can serve it.
func (g *Gateway) registerOutlineTool(server *mcp.Server, resolved serveContext) {
	if resolved.status.ServiceAddress == "" {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_outline",
		Description: "List what one file declares: functions, methods, types, fields, and constants, " +
			"each with the lines and bytes it occupies and the declaration that encloses it. " +
			"Also reports whether the file parses, which is how to check an edit before running anything. " +
			"The answer describes the file as it is on disk and needs no index, so a file saved a moment " +
			"ago is described correctly. " +
			"Declarations are what the syntax says: nothing is type-resolved and nothing outside this file " +
			"is consulted. " +
			"Paths are project-relative; the scope comes from the endpoint, and an absolute or escaping " +
			"path is refused.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input fileOutlineInput) (*mcp.CallToolResult, fileOutlineOutput, error) {
		outline, err := g.searchClient(resolved).Outline(ctx, input.Path)
		if err != nil {
			return nil, fileOutlineOutput{}, asSearchToolError(err)
		}
		return nil, fileOutlineOutput{
			ProjectID:   resolved.project.ID,
			Path:        outline.Path,
			Language:    outline.Language,
			Bytes:       outline.Bytes,
			Lines:       outline.Lines,
			Digest:      outline.Digest,
			Symbols:     outline.Symbols,
			Syntax:      outline.Syntax,
			ScopeSource: "route_and_registry",
			Root:        projectstack.WorkspaceMount,
			Provenance: outlineProvenance{
				Backend:       symbolBackend,
				SchemaVersion: outline.SchemaVersion,
				Precision:     precisionSyntax,
				ReadAt:        g.options.Now().UTC().Format(time.RFC3339),
			},
		}, nil
	})
}

// searchClient builds a client for the resolved project's published service.
func (g *Gateway) searchClient(resolved serveContext) *codeintel.Client {
	if g.options.NewSearchClient != nil {
		return g.options.NewSearchClient(resolved.status.ServiceAddress)
	}
	return codeintel.New(resolved.status.ServiceAddress)
}

// searchToolError carries a typed failure out through a tool result.
//
// A tool error reaches the model as text, so the text has to contain everything
// the model needs: what happened, whether waiting helps, and what to do. The
// alternative, an opaque "tool failed", makes an agent guess.
type searchToolError struct {
	code      string
	message   string
	action    string
	retryable bool
}

func (e *searchToolError) Error() string {
	parts := []string{e.code + ": " + terminated(e.message)}
	if e.retryable {
		parts = append(parts, "This is retryable.")
	}
	if e.action != "" {
		parts = append(parts, terminated(e.action))
	}
	return strings.Join(parts, " ")
}

// terminated ends a sentence that does not end itself.
//
// The parts above are joined with a space, and many of these messages finish on a
// quoted path or a backticked pattern, which is exactly where a reader loses the
// seam: "missing closing ]: `[unclosed` Correct the request and try again." reads
// as though Correct were part of the pattern. The text is what an agent acts on,
// so it has to parse on the first pass.
func terminated(text string) string {
	trimmed := strings.TrimRight(text, " ")
	if trimmed == "" {
		return trimmed
	}
	switch trimmed[len(trimmed)-1] {
	// Already punctuated. Appending to a colon or a semicolon would be worse than
	// leaving the seam, because it produces ":." rather than a sentence.
	case '.', '!', '?', ':', ';':
		return trimmed
	}
	return trimmed + "."
}

func asSearchToolError(err error) error {
	var typed *codeintel.Error
	if !errors.As(err, &typed) {
		return &searchToolError{code: codeintel.CodeInternalError, message: err.Error()}
	}
	return &searchToolError{
		code:      typed.Code,
		message:   typed.Message,
		action:    typed.Action,
		retryable: typed.Retryable,
	}
}

// bearerToken extracts the credential from an Authorization header, tolerating
// the case variation clients differ on.
func bearerToken(header string) string {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

// newRequestID produces a correlation identifier for local logs. It is not a
// security boundary, so a short random value is enough.
func newRequestID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + base64.RawURLEncoding.EncodeToString(raw)
}
