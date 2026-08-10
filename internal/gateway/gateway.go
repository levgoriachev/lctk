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
	// ExtendedTools also serves the semantic and graph-traversal tools. The
	// default surface is the five exploration tools ADR-0031 settled on; the
	// extended set stays available to an owner who turns it on, without taxing
	// every session's tool catalog.
	ExtendedTools bool
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
