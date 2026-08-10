package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/lev-goryachev/lctk/internal/daemon"
	"github.com/lev-goryachev/lctk/internal/projectauth"
	"github.com/lev-goryachev/lctk/internal/projectmanifest"
	"github.com/lev-goryachev/lctk/internal/projectregistration"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

const projectUsage = `Usage:
  lctk project add [--profile minimal|full] [--json] PATH
  lctk project status [--json] [PROJECT]
  lctk project start [--wait DURATION] [--yes] [--json] PROJECT
  lctk project stop [--json] PROJECT
  lctk project restart [--wait DURATION] [--yes] [--json] PROJECT
  lctk project remove [--json] PROJECT
  lctk project reindex [--full] [--json] PROJECT
  lctk project watch [--follow] [--json] PROJECT
  lctk project resources [--mode quiet|normal|fast|default] [--json] PROJECT
  lctk project commands [--approve NAME] [--revoke NAME] [--image IMAGE]
                        [--network none|full] [--json] PROJECT

PROJECT accepts a project id, a project name, an unambiguous id prefix, or the
path of a registered folder.
`

// defaultStartWait bounds how long a start waits for the stack to report healthy
// before returning the last observed state. A caller that receives a still
// starting state can retry.
const defaultStartWait = 90 * time.Second

// projectView is the machine-readable shape of one registration.
//
// It is derived from the stored record rather than being the stored record, so
// that on-disk schema changes and command output can evolve separately.
type projectView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	Profile         string `json:"profile"`
	RegisteredAt    string `json:"registered_at"`
	PathAvailable   bool   `json:"path_available"`
	ManifestPresent bool   `json:"manifest_present"`
	ManifestLocal   bool   `json:"manifest_local_override,omitempty"`
	// State is the runtime lifecycle state. It is reported as unknown with a
	// detail rather than failing when the container runtime cannot be reached, so
	// that registry information stays available while the managed runtime is stopped.
	State     string `json:"state"`
	Health    string `json:"health,omitempty"`
	Retryable bool   `json:"retryable"`
	Container string `json:"container,omitempty"`
	Network   string `json:"network,omitempty"`
	Volume    string `json:"volume,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	// ServiceAddress is where the project's code-intelligence service is
	// published on loopback. The runtime assigns the port, so it changes across a
	// restart and is reported rather than configured.
	ServiceAddress string   `json:"service_address,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

func viewOf(p projectregistry.Project) projectView {
	_, err := os.Stat(p.Path)
	view := projectView{
		ID:              p.ID,
		Name:            p.Name,
		Path:            p.Path,
		Profile:         string(p.Profile),
		RegisteredAt:    p.RegisteredAt.Format("2006-01-02T15:04:05Z"),
		PathAvailable:   err == nil,
		ManifestPresent: p.ManifestPresent,
		State:           string(projectstack.StateUnknown),
	}
	// Resource names are a pure function of the project id, so they are always
	// reportable, including while the container runtime is unreachable.
	if names, err := projectstack.DeriveNames(p.ID); err == nil {
		view.Network = names.Network
		view.Volume = names.Volume
	}

	// The manifest is re-read rather than taken from the registry, because the
	// registry records what was there at registration and a manifest added since
	// then is exactly what somebody asking about a project wants to know about.
	//
	// Skipped when the path is gone: a project on a disconnected drive would make
	// every listing wait for the filesystem to give up, and "the path is not
	// available" is already the answer being reported.
	if view.PathAvailable {
		if manifest, err := projectmanifest.Load(p.Path); err == nil {
			view.ManifestPresent = manifest.TrackedPresent
			view.ManifestLocal = manifest.LocalPresent
			view.Warnings = manifest.Warnings
		} else {
			view.Warnings = append(view.Warnings, "manifest could not be read: "+err.Error())
		}
	}
	return view
}

// withRuntime fills the runtime fields of a view.
//
// A runtime failure is folded into the view rather than returned, because a
// caller asking for status wants the registry answer even when Docker is
// unavailable, and the state field already says the answer is unknown.
func withRuntime(view projectView, status projectstack.Status, err error) projectView {
	if status.State != "" {
		view.State = string(status.State)
	}
	view.Health = status.Health
	view.Container = status.Container
	view.Network = status.Network
	view.Volume = status.Volume
	view.ServiceAddress = status.ServiceAddress
	view.Retryable = status.State.Retryable()
	if status.Detail != "" {
		view.Detail = status.Detail
	} else if err != nil {
		view.Detail = err.Error()
	}
	return view
}

// newStackManager constructs the container-stack manager. It is a variable so
// tests can substitute a scripted runtime instead of driving real containers,
// which keeps command tests fast and identical whether or not Docker is present.
var newStackManager = projectstack.NewManager

// applyStackStatus fills the runtime fields of a view from a live query.
func applyStackStatus(manager *projectstack.Manager, project projectregistry.Project, view projectView) projectView {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := manager.Status(ctx, project)
	return withRuntime(view, status, err)
}

func runProject(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, projectUsage)
		return errors.New("a project subcommand is required")
	}
	switch args[0] {
	case "add":
		return runProjectAdd(args[1:], stdout, stderr)
	case "status":
		return runProjectStatus(args[1:], stdout)
	case "start":
		return runProjectLifecycle("start", args[1:], stdout)
	case "stop":
		return runProjectLifecycle("stop", args[1:], stdout)
	case "restart":
		return runProjectLifecycle("restart", args[1:], stdout)
	case "remove":
		return runProjectRemove(args[1:], stdout)
	case "reindex":
		return runProjectReindex(args[1:], stdout)
	case "watch":
		return runProjectWatch(context.Background(), args[1:], stdout)
	case "resources":
		return runProjectResources(args[1:], stdout)
	case "commands":
		return runProjectCommands(args[1:], stdout)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, projectUsage)
		return nil
	default:
		fmt.Fprint(stderr, projectUsage)
		return fmt.Errorf("unknown project subcommand %q", args[0])
	}
}

// runProjectAdd registers a folder. It starts no containers, networks, volumes,
// or indexing: Slice 1.1 is registration only.
func runProjectAdd(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("project add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "", "capability profile: minimal or full")
	asJSON := flags.Bool("json", false, "write the registration as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: lctk project add [--profile minimal|full] [--json] PATH")
	}

	registered, err := projectregistration.Register(flags.Arg(0), projectregistry.Profile(*profile))
	if err != nil {
		return err
	}
	project, manifest := registered.Project, registered.Manifest

	view := viewOf(project)
	view.ManifestLocal = manifest.LocalPresent
	view.Warnings = manifest.Warnings
	view.Endpoint = fmt.Sprintf("http://%s/projects/%s/mcp", daemon.DefaultAddress, project.ID)

	if *asJSON {
		return writeJSON(stdout, view)
	}

	fmt.Fprintf(stdout, "Registered %s\n", view.ID)
	fmt.Fprintf(stdout, "  path:     %s\n", view.Path)
	fmt.Fprintf(stdout, "  profile:  %s\n", view.Profile)
	fmt.Fprintf(stdout, "  manifest: %s\n", manifestSummary(manifest))
	fmt.Fprintf(stdout, "  endpoint: %s\n", view.Endpoint)
	fmt.Fprint(stdout, "No client was authorized. Add the MCP URL to a client and approve its OAuth request in LCTK.\n")
	fmt.Fprint(stdout, "No services were started. Use lctk project start to run it.\n")
	for _, warning := range view.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	return nil
}

func manifestSummary(result projectmanifest.Result) string {
	switch {
	case result.TrackedPresent && result.LocalPresent:
		return projectmanifest.FileName + " with a local override"
	case result.TrackedPresent:
		return projectmanifest.FileName
	case result.LocalPresent:
		return projectmanifest.LocalFileName + " only"
	default:
		return "none"
	}
}

// runProjectStatus reports one project, or every project when no reference is
// given.
func runProjectStatus(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("project status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write status as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: lctk project status [--json] [PROJECT]")
	}

	registry, err := projectregistry.Load()
	if err != nil {
		return err
	}
	manager := newStackManager()

	if flags.NArg() == 1 {
		project, err := registry.Resolve(flags.Arg(0))
		if err != nil {
			return err
		}
		view := applyStackStatus(manager, project, viewOf(project))
		if *asJSON {
			return writeJSON(stdout, view)
		}
		return writeProjectDetail(stdout, view)
	}

	projects := registry.List()
	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		views = append(views, applyStackStatus(manager, p, viewOf(p)))
	}

	if *asJSON {
		return writeJSON(stdout, views)
	}
	if len(views) == 0 {
		fmt.Fprint(stdout, "No projects are registered. Use lctk project add PATH.\n")
		return nil
	}
	return writeProjectTable(stdout, views)
}

// runProjectLifecycle implements start, stop, and restart, which differ only in
// which stack operation they invoke.
func runProjectLifecycle(action string, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("project "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write the resulting status as JSON")
	var (
		wait    time.Duration
		proceed bool
	)
	if action != "stop" {
		flags.DurationVar(&wait, "wait", defaultStartWait, "how long to wait for the stack to report healthy")
		flags.BoolVar(&proceed, "yes", false, "start even when the volume is short of space")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: lctk project %s [--json] PROJECT", action)
	}

	registry, err := projectregistry.Load()
	if err != nil {
		return err
	}
	project, err := registry.Resolve(flags.Arg(0))
	if err != nil {
		return err
	}

	manager := newStackManager()
	// The budget covers the operation plus the health wait, with headroom so the
	// caller sees the lifecycle timeout rather than a context deadline.
	ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Minute)
	defer cancel()

	if action == "start" || action == "restart" {
		if err := warnIfDiskIsTight(ctx, project, stdout, proceed); err != nil {
			return err
		}
	}

	var status projectstack.Status
	var actionErr error
	switch action {
	case "start":
		status, actionErr = manager.Start(ctx, project, wait)
	case "stop":
		status, actionErr = manager.Stop(ctx, project)
	case "restart":
		status, actionErr = manager.Restart(ctx, project, wait)
	default:
		return fmt.Errorf("unknown lifecycle action %q", action)
	}

	view := withRuntime(viewOf(project), status, actionErr)
	if *asJSON {
		// The status is written even on failure, because a caller needs the state
		// and whether retrying is worthwhile in order to decide what to do next.
		if err := writeJSON(stdout, view); err != nil {
			return err
		}
		return actionErr
	}
	if actionErr != nil {
		return actionErr
	}
	fmt.Fprintf(stdout, "%s %s\n", lifecyclePastTense[action], project.ID)
	return writeProjectDetail(stdout, view)
}

var lifecyclePastTense = map[string]string{
	"start":   "Started",
	"stop":    "Stopped",
	"restart": "Restarted",
}

func runProjectRemove(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("project remove", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write the removed registration as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: lctk project remove [--json] PROJECT")
	}

	registry, err := projectregistry.Load()
	if err != nil {
		return err
	}
	project, err := registry.Remove(flags.Arg(0))
	if err != nil {
		return err
	}

	// Per docs/project-lifecycle.md, remove releases runtime resources but never
	// deletes persistent data. Stopping is best effort: an unreachable runtime
	// must not block deregistration, so the outcome is reported instead.
	view := viewOf(project)
	stopDetail := ""
	manager := newStackManager()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if status, stopErr := manager.Stop(ctx, project); stopErr != nil {
		if errors.Is(stopErr, projectstack.ErrRuntimeUnavailable) {
			stopDetail = "container runtime unavailable, so the stack was not stopped"
		} else {
			stopDetail = "stopping the stack failed: " + stopErr.Error()
		}
		view.Detail = stopDetail
	} else {
		view = withRuntime(view, status, nil)
	}

	if err := registry.Save(); err != nil {
		return err
	}

	// A credential must not outlive the exact project resource it covers.
	authorizations, authorizationErr := projectauth.Open()
	if authorizationErr == nil {
		authorizationErr = authorizations.RevokeForProject(project.ID)
	}
	if authorizationErr != nil {
		view.Warnings = append(view.Warnings, "OAuth authorizations could not be updated: "+authorizationErr.Error())
	}

	if *asJSON {
		return writeJSON(stdout, view)
	}
	fmt.Fprintf(stdout, "Removed %s (%s)\n", project.ID, project.Path)
	for _, warning := range view.Warnings {
		fmt.Fprintf(stdout, "  warning:  %s\n", warning)
	}
	if stopDetail != "" {
		fmt.Fprintf(stdout, "  warning:  %s\n", stopDetail)
	}
	fmt.Fprint(stdout, "Project data on disk was not deleted.\n")
	fmt.Fprintf(stdout, "The project volume %s was kept; deleting it is a separate purge.\n", view.Volume)
	return nil
}

func writeProjectTable(output io.Writer, views []projectView) error {
	writer := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tPROFILE\tSTATE\tPATH\tPATH STATE")
	for _, v := range views {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			v.ID, v.Name, v.Profile, v.State, v.Path, pathState(v))
	}
	return writer.Flush()
}

func writeProjectDetail(output io.Writer, v projectView) error {
	fmt.Fprintf(output, "%s\n", v.ID)
	fmt.Fprintf(output, "  name:       %s\n", v.Name)
	fmt.Fprintf(output, "  path:       %s\n", v.Path)
	fmt.Fprintf(output, "  path state: %s\n", pathState(v))
	fmt.Fprintf(output, "  profile:    %s\n", v.Profile)
	fmt.Fprintf(output, "  state:      %s\n", runtimeState(v))
	fmt.Fprintf(output, "  registered: %s\n", v.RegisteredAt)
	fmt.Fprintf(output, "  manifest:   %s\n", manifestState(v))
	if v.Container != "" {
		fmt.Fprintf(output, "  container:  %s\n", v.Container)
	}
	if v.Volume != "" {
		fmt.Fprintf(output, "  volume:     %s\n", v.Volume)
	}
	if v.ServiceAddress != "" {
		fmt.Fprintf(output, "  service:    %s\n", v.ServiceAddress)
	}
	if v.Detail != "" {
		fmt.Fprintf(output, "  detail:     %s\n", v.Detail)
	}
	for _, warning := range v.Warnings {
		fmt.Fprintf(output, "  warning:    %s\n", warning)
	}
	return nil
}

// runtimeState renders the lifecycle state, noting when retrying is worthwhile so
// the human output carries the same advice as the JSON field.
func runtimeState(v projectView) string {
	if v.Health != "" {
		if v.Retryable {
			return fmt.Sprintf("%s (health %s, retryable)", v.State, v.Health)
		}
		return fmt.Sprintf("%s (health %s)", v.State, v.Health)
	}
	if v.Retryable {
		return v.State + " (retryable)"
	}
	return v.State
}

func pathState(v projectView) string {
	if v.PathAvailable {
		return "available"
	}
	return "unavailable"
}

func manifestState(v projectView) string {
	switch {
	case v.ManifestPresent && v.ManifestLocal:
		return projectmanifest.FileName + " with a local override"
	case v.ManifestPresent:
		return projectmanifest.FileName
	case v.ManifestLocal:
		return projectmanifest.LocalFileName + " only"
	default:
		return "none"
	}
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
