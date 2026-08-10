package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/lev-goryachev/lctk/internal/adminapi"
	"github.com/lev-goryachev/lctk/internal/adminsession"
	"github.com/lev-goryachev/lctk/internal/auditlog"
	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/daemonstate"
	"github.com/lev-goryachev/lctk/internal/gateway"
	"github.com/lev-goryachev/lctk/internal/gitinfo"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/localapi"
	"github.com/lev-goryachev/lctk/internal/logring"
	"github.com/lev-goryachev/lctk/internal/mcpserver"
	"github.com/lev-goryachev/lctk/internal/projectauth"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/runner"
	"github.com/lev-goryachev/lctk/internal/watchsupervisor"
)

const DefaultAddress = localapi.DefaultAddress

type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// NewHandler builds the daemon's HTTP surface with production defaults.
func NewHandler() http.Handler {
	// The project-scoped endpoint from ADR-0001. Registry and OAuth state are read per
	// request, so a project registered while the daemon runs becomes reachable
	// without a restart. Lifecycle gating is on, because a request served by a
	// stopped project would be answering about a stack that is not there.
	return NewHandlerWithGateway(gateway.Options{RequireRunning: true})
}

// NewHandlerWithGateway builds the daemon's HTTP surface with explicit gateway
// options. It exists so tests can supply an in-memory registry, OAuth store, and
// status probe instead of requiring a container runtime.
func NewHandlerWithGateway(options gateway.Options) http.Handler {
	mux := http.NewServeMux()
	registerCore(mux)
	gateway.New(options).Register(mux)
	return mux
}

// registerCore attaches the endpoints every daemon has, whatever else it serves.
func registerCore(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(Health{
			Status:  "ok",
			Version: buildinfo.Version,
		})
	})
	mux.Handle("/mcp", mcpserver.NewHTTPHandler())
}

// changeReporter adapts the watch supervisor to the gateway's vocabulary, so the
// gateway does not need to know that a supervisor exists.
func changeReporter(supervisor *watchsupervisor.Supervisor) gateway.ChangeReporter {
	return func(projectID string) (gateway.ChangeState, bool) {
		view, ok := supervisor.View(projectID)
		if !ok {
			return gateway.ChangeState{}, false
		}
		state := gateway.ChangeState{
			Watching:        view.Watching,
			Pending:         view.Pending,
			Indexing:        view.Indexing,
			LastEventAt:     view.LastEventAt,
			DebounceSeconds: view.DebounceSeconds,
		}
		if view.Gap != nil {
			state.GapReason = view.Gap.Reason
		}
		return state, true
	}
}

func Run(ctx context.Context, address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	home, err := lctkhome.EnsureDir()
	if err != nil {
		listener.Close()
		return err
	}
	removeDaemonState, err := daemonstate.Register(home)
	if err != nil {
		listener.Close()
		return fmt.Errorf("record daemon identity: %w", err)
	}
	defer removeDaemonState()

	// Recent log records are kept in memory so the admin surface can show what
	// just happened without an operator having to find a file.
	history := logring.New(slog.NewTextHandler(os.Stderr, nil), logring.DefaultCapacity)
	logger := slog.New(history)
	slog.SetDefault(logger)

	// The watcher lives in the daemon rather than in the CLI because it must
	// outlive any one command, and on the host rather than in the container
	// because that is where the native events are reliable.
	supervisor := watchsupervisor.New(watchsupervisor.Options{Logger: logger})
	go supervisor.Run(ctx)

	// A log that cannot be written is reported and then set aside. Refusing to
	// run anything because the record is unwritable would be worse than running
	// with a gap in it, and the gap is visible either way.
	audit, err := auditlog.New()
	if err != nil {
		logger.Warn("commands will run without an audit record", slog.String("error", err.Error()))
		audit = nil
	}

	// OAuth is daemon-owned so pending approvals, authorization codes, refresh
	// rotation, the gateway, and the native administrator share one locked state.
	authorizations, err := projectauth.Open()
	if err != nil {
		return fmt.Errorf("open the OAuth authorization store: %w", err)
	}

	sessions, err := adminsession.New(adminsession.Options{})
	if err != nil {
		return fmt.Errorf("prepare the admin session: %w", err)
	}
	defer sessions.Close()

	mux := http.NewServeMux()
	registerCore(mux)
	gateway.New(gateway.Options{
		Authorizations: func() (*projectauth.Store, error) { return authorizations, nil },
		RequireRunning: true,
		Logger:         logger,
		Wake:           supervisor.Wake,
		Changes:        changeReporter(supervisor),
		Flush:          supervisor.Flush,
		Git:            gitinfo.New(),
		Runner:         runner.New(),
		Budget:         projectstack.NewManager().Budget,
		Audit:          audit,
	}).Register(mux)
	projectauth.NewHTTPServer(authorizations, projectregistry.Load, time.Now).Register(mux)
	adminapi.New(adminapi.Options{
		Sessions:       sessions,
		Authorizations: authorizations,
		Watch:          supervisor.View,
		Logs:           history.Records,
	}).Register(mux)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve daemon API: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down daemon API: %w", err)
		}
		return nil
	}
}
