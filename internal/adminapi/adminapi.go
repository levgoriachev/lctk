// Package adminapi serves the local administrator's surface.
//
// It is separate from the project MCP endpoint by construction, not by
// convention. [ADR-0001] binds a project endpoint to one project authorization;
// this surface is about the machine, sees every project, and would break that
// binding if a coding session could reach it. So they share a listener and
// nothing else: no admin handler accepts a project token, and no project route
// consults an admin session.
//
// The session rules — a spent-once exchange code, a SameSite=Strict cookie, a
// loopback Host check, and a CSRF header on anything that changes state — live in
// internal/adminsession and are recorded in [ADR-0016].
//
// [ADR-0001]: ../../docs/adr/0001-route-bound-project-scope.md
// [ADR-0016]: ../../docs/adr/0016-admin-surface-and-local-session.md
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/adminsession"
	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/diskspace"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/logring"
	"github.com/lev-goryachev/lctk/internal/projectauth"
	"github.com/lev-goryachev/lctk/internal/projectregistration"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/runtimeapi"
	"github.com/lev-goryachev/lctk/internal/watchsupervisor"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

// actionTimeout bounds a lifecycle action started from the native window, so a
// request cannot hang on a container runtime that has stopped answering.
const actionTimeout = 3 * time.Minute

// Options wires the API to the daemon's components. Every one is injectable so
// the surface can be tested without a container runtime.
type Options struct {
	Sessions          *adminsession.Store
	Registry          func() (*projectregistry.Registry, error)
	Authorizations    *projectauth.Store
	Stack             Lifecycle
	Settings          func() (hostsettings.Settings, error)
	Watch             func(projectID string) (watchsupervisor.View, bool)
	Probe             func(ctx context.Context) (runtimeapi.Status, error)
	Inference         func(ctx context.Context) (inference.Status, error)
	Logs              func() []logring.Record
	Now               func() time.Time
	Register          func(path string, profile projectregistry.Profile) (projectregistration.Result, error)
	LaunchUninstaller func() error
}

// Lifecycle is the part of the stack manager the admin surface drives.
type Lifecycle interface {
	Status(ctx context.Context, project projectregistry.Project) (projectstack.Status, error)
	Start(ctx context.Context, project projectregistry.Project, wait time.Duration) (projectstack.Status, error)
	Stop(ctx context.Context, project projectregistry.Project) (projectstack.Status, error)
	Restart(ctx context.Context, project projectregistry.Project, wait time.Duration) (projectstack.Status, error)
}

// Server is the admin surface.
type Server struct {
	options            Options
	authorizationError error
}

// New builds a server. Sessions is required; everything else has a default.
func New(options Options) *Server {
	if options.Registry == nil {
		options.Registry = projectregistry.Load
	}
	var authorizationError error
	if options.Authorizations == nil {
		options.Authorizations, authorizationError = projectauth.Open()
	}
	if options.Stack == nil {
		options.Stack = projectstack.NewManager()
	}
	if options.Settings == nil {
		options.Settings = hostsettings.Load
	}
	if options.Probe == nil {
		options.Probe = runtimeapi.Probe
	}
	if options.Inference == nil {
		options.Inference = func(ctx context.Context) (inference.Status, error) {
			manager, err := inference.NewRuntimeManager()
			if err != nil {
				return inference.Status{}, err
			}
			return manager.Status(ctx)
		}
	}
	if options.Logs == nil {
		options.Logs = func() []logring.Record { return nil }
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Register == nil {
		options.Register = projectregistration.Register
	}
	if options.LaunchUninstaller == nil {
		options.LaunchUninstaller = launchUninstaller
	}
	return &Server{options: options, authorizationError: authorizationError}
}

// Register attaches the admin surface to a mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/session", s.handleSignIn)
	mux.HandleFunc("DELETE /admin/session", s.handleSignOut)

	mux.HandleFunc("GET /admin/api/overview", s.read(s.handleOverview))
	mux.HandleFunc("GET /admin/api/projects", s.read(s.handleProjects))
	mux.HandleFunc("POST /admin/api/projects", s.write(s.handleProjectAdd))
	mux.HandleFunc("GET /admin/api/authorizations", s.read(s.handleAuthorizations))
	mux.HandleFunc("GET /admin/api/oauth/requests", s.read(s.handleAuthorizationRequests))
	mux.HandleFunc("GET /admin/api/logs", s.read(s.handleLogs))
	mux.HandleFunc("POST /admin/api/uninstall", s.write(s.handleUninstall))

	mux.HandleFunc("POST /admin/api/projects/{id}/{action}", s.write(s.handleProjectAction))
	mux.HandleFunc("POST /admin/api/oauth/requests/{id}/{decision}", s.write(s.handleAuthorizationDecision))
	mux.HandleFunc("DELETE /admin/api/authorizations/{id}", s.write(s.handleRevokeAuthorization))
}

func (s *Server) handleUninstall(w http.ResponseWriter, _ *http.Request) {
	if err := s.options.LaunchUninstaller(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "The uninstall choice dialog was opened."})
}

func launchUninstaller() error {
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	command := exec.Command(filepath.Join(home, "bin", "lctk-setup.exe"), "--uninstall")
	windowsprocess.HideConsole(command)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

type addProjectRequest struct {
	Path    string `json:"path"`
	Profile string `json:"profile"`
}

func (s *Server) handleProjectAdd(w http.ResponseWriter, r *http.Request) {
	var request addProjectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The request body is not valid JSON."})
		return
	}
	profile := projectregistry.Profile(strings.ToLower(strings.TrimSpace(request.Profile)))
	if profile != "" && profile != projectregistry.ProfileMinimal && profile != projectregistry.ProfileFull {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Profile must be minimal or full."})
		return
	}
	registered, err := s.options.Register(strings.TrimSpace(request.Path), profile)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id": registered.Project.ID, "name": registered.Project.Name, "path": registered.Project.Path,
	})
}

// read wraps a handler that only reports. It still requires a session: the
// project list is itself something a stranger should not be able to read.
func (s *Server) read(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.options.Sessions.Authenticate(r); err != nil {
			refuse(w, err)
			return
		}
		next(w, r)
	}
}

// write wraps a handler that changes something, and additionally requires the
// CSRF header a cross-origin page cannot produce.
func (s *Server) write(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.options.Sessions.Authorize(r); err != nil {
			refuse(w, err)
			return
		}
		next(w, r)
	}
}

func refuse(w http.ResponseWriter, err error) {
	status := http.StatusUnauthorized
	message := "A native administrator session is required. Open LCTK again."
	if errors.Is(err, adminsession.ErrForeignHost) {
		// Naming this precisely matters: it is the one refusal that means
		// something is actively wrong rather than that a session expired.
		status = http.StatusForbidden
		message = "This request did not name a loopback host."
	}
	writeJSON(w, status, map[string]string{"error": message})
}

type signInRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	if !adminsession.LoopbackHost(r.Host) {
		refuse(w, adminsession.ErrForeignHost)
		return
	}

	var request signInRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The request body is not valid JSON."})
		return
	}

	token, csrf, err := s.options.Sessions.Exchange(strings.TrimSpace(request.Code))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "That code is not valid, or has already been used. Open LCTK again.",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"session": token, "csrf": csrf})
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if token := adminsession.Token(r); token != "" {
		s.options.Sessions.Revoke(token)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

type overview struct {
	Version string `json:"version"`
	// Runtime is the container runtime diagnostic: this is the "doctor" an
	// operator needs, and it is the first thing that is wrong when anything is.
	Runtime      runtimeView           `json:"runtime"`
	Inference    inference.Status      `json:"inference"`
	Home         string                `json:"home"`
	Settings     hostsettings.Settings `json:"settings"`
	ServerTime   string                `json:"server_time"`
	ProjectCount int                   `json:"project_count"`
}

type runtimeView struct {
	Available bool   `json:"available"`
	Provider  string `json:"provider,omitempty"`
	Version   string `json:"version,omitempty"`
	OSType    string `json:"os_type,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	view := overview{
		Version:    buildinfo.Version,
		ServerTime: s.options.Now().UTC().Format(time.RFC3339),
	}
	if home, err := lctkhome.Dir(); err == nil {
		view.Home = home
	}
	if settings, err := s.options.Settings(); err == nil {
		view.Settings = settings
	}
	if registry, err := s.options.Registry(); err == nil {
		view.ProjectCount = registry.Len()
	}

	probeCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if status, err := s.options.Probe(probeCtx); err != nil {
		view.Runtime.Detail = err.Error()
	} else {
		view.Runtime = runtimeView{Available: true, Provider: status.Provider, Version: status.Version, OSType: status.OSType}
	}
	if status, err := s.options.Inference(probeCtx); err != nil {
		status.Detail = err.Error()
		view.Inference = status
	} else {
		view.Inference = status
	}
	writeJSON(w, http.StatusOK, view)
}

type projectView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	// Path is shown because this surface is the machine owner's, and they are the
	// one person who is entitled to see where their projects live. A project
	// endpoint never reveals it.
	Path    string      `json:"path"`
	State   string      `json:"state"`
	Health  string      `json:"health,omitempty"`
	Detail  string      `json:"detail,omitempty"`
	Mode    string      `json:"mode"`
	Index   *indexView  `json:"index,omitempty"`
	Changes *changeView `json:"changes,omitempty"`
	Disk    diskView    `json:"disk"`
}

type indexView struct {
	Ready      bool               `json:"ready"`
	Indexing   bool               `json:"indexing"`
	Generation uint64             `json:"generation"`
	FileCount  int                `json:"file_count"`
	IndexedAt  string             `json:"indexed_at,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Semantic   *semanticIndexView `json:"semantic,omitempty"`
	Graph      *graphIndexView    `json:"graph,omitempty"`
}

// semanticIndexView exposes durable publication, live progress, the last failed
// attempt, and a server-detected stall independently. An exact index being ready
// must never make the Admin window imply that semantic search is ready too.
type semanticIndexView struct {
	Ready          bool   `json:"ready"`
	Indexing       bool   `json:"indexing"`
	Generation     uint64 `json:"generation"`
	FileCount      int    `json:"file_count"`
	ChunkCount     int    `json:"chunk_count"`
	Freshness      string `json:"freshness"`
	Reason         string `json:"reason,omitempty"`
	ChunksTotal    int    `json:"chunks_total,omitempty"`
	ChunksEmbedded int    `json:"chunks_embedded,omitempty"`
	ChunksReused   int    `json:"chunks_reused,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	ProgressAt     string `json:"progress_at,omitempty"`
	Stalled        bool   `json:"stalled,omitempty"`
	StallSeconds   int64  `json:"stall_seconds,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

type graphIndexView struct {
	Ready       bool   `json:"ready"`
	Generation  uint64 `json:"generation"`
	FileCount   int    `json:"file_count"`
	NodeCount   int    `json:"node_count"`
	ImportCount int    `json:"import_count"`
	CallCount   int    `json:"call_count"`
	Freshness   string `json:"freshness"`
	Reason      string `json:"reason,omitempty"`
}

type changeView struct {
	Watching    bool   `json:"watching"`
	Pending     int    `json:"pending"`
	Complete    bool   `json:"complete"`
	GapReason   string `json:"gap_reason,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	Directories int    `json:"directories"`
}

type diskView struct {
	SourceBytes int64  `json:"source_bytes"`
	IndexBytes  int64  `json:"index_bytes"`
	Human       string `json:"human"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	registry, err := s.options.Registry()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	settings, _ := s.options.Settings()
	projects := registry.List()
	views := make([]projectView, 0, len(projects))

	for _, project := range projects {
		effective := settings.Resources.WithProjectMode(hostsettings.Mode(project.ResourceMode))
		view := projectView{
			ID:      project.ID,
			Name:    project.Name,
			Profile: string(project.Profile),
			Path:    project.Path,
			State:   string(projectstack.StateUnknown),
			Mode:    string(effective.Mode),
		}

		statusCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		status, statusErr := s.options.Stack.Status(statusCtx, project)
		cancel()
		if statusErr != nil {
			view.Detail = statusErr.Error()
		} else {
			view.State = string(status.State)
			view.Health = status.Health
			if status.Detail != "" {
				view.Detail = status.Detail
			}
		}

		if statusErr == nil && status.ServiceAddress != "" {
			indexCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			indexStatus, indexErr := codeintel.New(status.ServiceAddress).Status(indexCtx)
			cancel()
			if indexErr == nil {
				view.Index = &indexView{
					Ready:      indexStatus.Ready,
					Indexing:   indexStatus.Indexing,
					Generation: indexStatus.Generation,
					FileCount:  indexStatus.FileCount,
					IndexedAt:  indexStatus.IndexedAt,
					Reason:     indexStatus.Reason,
				}
				if semantic := indexStatus.Semantic; semantic != nil {
					view.Index.Semantic = &semanticIndexView{
						Ready: semantic.Ready, Indexing: semantic.Indexing, Generation: semantic.Generation,
						FileCount: semantic.FileCount, ChunkCount: semantic.ChunkCount, Freshness: semantic.Freshness,
						Reason: semantic.Reason, ChunksTotal: semantic.ChunksTotal,
						ChunksEmbedded: semantic.ChunksEmbedded, ChunksReused: semantic.ChunksReused,
						StartedAt: semantic.StartedAt, ProgressAt: semantic.ProgressAt,
						Stalled: semantic.Stalled, StallSeconds: semantic.StallSeconds, LastError: semantic.LastError,
					}
				}
				if graph := indexStatus.Graph; graph != nil {
					view.Index.Graph = &graphIndexView{
						Ready: graph.Ready, Generation: graph.Generation, FileCount: graph.FileCount,
						NodeCount: graph.NodeCount, ImportCount: graph.ImportCount, CallCount: graph.CallCount,
						Freshness: graph.Freshness, Reason: graph.Reason,
					}
				}
				view.Disk = diskView{
					SourceBytes: indexStatus.SourceBytes,
					IndexBytes:  indexStatus.IndexBytes,
					Human:       diskspace.Human(indexStatus.IndexBytes),
				}
			}
		}

		if s.options.Watch != nil {
			if watched, ok := s.options.Watch(project.ID); ok {
				change := &changeView{
					Watching:    watched.Watching,
					Pending:     watched.Pending,
					Complete:    watched.Gap == nil,
					LastError:   watched.LastError,
					Directories: watched.Directories,
				}
				if watched.Gap != nil {
					change.GapReason = watched.Gap.Reason
				}
				view.Changes = change
			}
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": views})
}

type modeRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleProjectAction(w http.ResponseWriter, r *http.Request) {
	registry, err := s.options.Registry()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Lookup is by exact identifier. The convenience matching the CLI offers must
	// not decide which project a button press affects.
	var project projectregistry.Project
	found := false
	for _, candidate := range registry.List() {
		if candidate.ID == r.PathValue("id") {
			project, found = candidate, true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No such project."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	switch r.PathValue("action") {
	case "start":
		status, err := s.options.Stack.Start(ctx, project, 90*time.Second)
		s.report(w, status, err)
	case "stop":
		status, err := s.options.Stack.Stop(ctx, project)
		s.report(w, status, err)
	case "restart":
		status, err := s.options.Stack.Restart(ctx, project, 90*time.Second)
		s.report(w, status, err)
	case "reindex":
		s.reindex(w, ctx, project)
	case "mode":
		s.setMode(w, r, registry, project)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Unknown action."})
	}
}

func (s *Server) report(w http.ResponseWriter, status projectstack.Status, err error) {
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(status.State), "health": status.Health})
}

func (s *Server) reindex(w http.ResponseWriter, ctx context.Context, project projectregistry.Project) {
	status, err := s.options.Stack.Status(ctx, project)
	if err != nil || status.ServiceAddress == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "The project is not running a code-intelligence service.",
		})
		return
	}
	indexStatus, err := codeintel.New(status.ServiceAddress).Reindex(ctx, true)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generation": indexStatus.Generation, "file_count": indexStatus.FileCount,
	})
}

func (s *Server) setMode(w http.ResponseWriter, r *http.Request,
	registry *projectregistry.Registry, project projectregistry.Project) {
	var request modeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The request body is not valid JSON."})
		return
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "default" {
		mode = ""
	}
	if !hostsettings.Mode(mode).Valid() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Unknown resource mode."})
		return
	}
	if err := registry.SetResourceMode(project.ID, mode); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := registry.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"mode": mode,
		"note": "Limits apply when a container is created; restart the project to apply this now.",
	})
}

type authorizationView struct {
	ID       string   `json:"id"`
	Client   string   `json:"client"`
	Project  string   `json:"project"`
	Scopes   []string `json:"scopes"`
	IssuedAt string   `json:"issued_at,omitempty"`
	Revoked  bool     `json:"revoked,omitempty"`
}

func (s *Server) handleAuthorizations(w http.ResponseWriter, _ *http.Request) {
	if s.authorizationError != nil || s.options.Authorizations == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OAuth authorizations are unavailable."})
		return
	}
	listed := s.options.Authorizations.List()
	views := make([]authorizationView, 0, len(listed))
	for _, authorization := range listed {
		view := authorizationView{ID: authorization.ID, Client: authorization.ClientName, Project: authorization.ProjectID, Scopes: authorization.Scopes, Revoked: authorization.Revoked}
		if !authorization.IssuedAt.IsZero() {
			view.IssuedAt = authorization.IssuedAt.UTC().Format(time.RFC3339)
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorizations": views})
}

type authorizationRequestView struct {
	ID          string   `json:"id"`
	Client      string   `json:"client"`
	Project     string   `json:"project"`
	RedirectURI string   `json:"redirect_uri"`
	Scopes      []string `json:"scopes"`
	ExpiresAt   string   `json:"expires_at"`
}

func (s *Server) handleAuthorizationRequests(w http.ResponseWriter, _ *http.Request) {
	if s.authorizationError != nil || s.options.Authorizations == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OAuth authorizations are unavailable."})
		return
	}
	pending := s.options.Authorizations.Pending(s.options.Now())
	views := make([]authorizationRequestView, 0, len(pending))
	for _, request := range pending {
		views = append(views, authorizationRequestView{ID: request.ID, Client: request.ClientName, Project: request.ProjectID, RedirectURI: request.RedirectURI, Scopes: request.Scopes, ExpiresAt: request.ExpiresAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": views})
}

func (s *Server) handleAuthorizationDecision(w http.ResponseWriter, r *http.Request) {
	if s.authorizationError != nil || s.options.Authorizations == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OAuth authorizations are unavailable."})
		return
	}
	var err error
	status := ""
	switch r.PathValue("decision") {
	case "approve":
		_, err = s.options.Authorizations.Approve(r.PathValue("id"), s.options.Now())
		status = "approved"
	case "deny":
		_, err = s.options.Authorizations.Deny(r.PathValue("id"), s.options.Now())
		status = "denied"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Decision must be approve or deny."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) handleRevokeAuthorization(w http.ResponseWriter, r *http.Request) {
	if s.authorizationError != nil || s.options.Authorizations == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OAuth authorizations are unavailable."})
		return
	}
	if _, err := s.options.Authorizations.Revoke(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"records": s.options.Logs()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// The admin surface must never be embedded or sniffed into something else.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
