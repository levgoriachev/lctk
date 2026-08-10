package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/auditlog"
	"github.com/lev-goryachev/lctk/internal/commandpolicy"
	"github.com/lev-goryachev/lctk/internal/projectmanifest"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/runner"
)

// Typed codes run_command reports.
const (
	CodeCommandUnknown      = "COMMAND_UNKNOWN"
	CodeCommandNotProposed  = "COMMAND_NOT_PROPOSED"
	CodeCommandNotApproved  = "COMMAND_NOT_APPROVED"
	CodeCommandChanged      = "COMMAND_CHANGED"
	CodeRunnerNotConfigured = "RUNNER_NOT_CONFIGURED"
	CodeRunnerUnavailable   = "RUNNER_UNAVAILABLE"
	CodeRunnerFailed        = "RUNNER_FAILED"
)

// CommandRunner executes an approved command. It is an interface so the tool can
// be tested without a container runtime.
type CommandRunner interface {
	Run(ctx context.Context, request runner.Request) (runner.Result, error)
}

// ManifestLoader reads a project's proposals. The manifest is read per request
// rather than cached, so a command edited in the repository loses its approval
// immediately rather than at the next daemon restart.
type ManifestLoader func(projectRoot string) (projectmanifest.Result, error)

// Auditor records what was executed.
type Auditor interface {
	Append(entry auditlog.Entry) error
}

type runCommandInput struct {
	Name string `json:"name"`
}

// registerRunTool adds run_command when the project has a runner configured.
//
// The tool takes a name and never a command line. That is the whole design: a
// client can run exactly the set of things a human read and approved, and adding
// to that set is an act only the machine owner can perform.
func (g *Gateway) registerRunTool(server *mcp.Server, resolved serveContext) {
	if g.options.Runner == nil {
		return
	}

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "run_command",
		Description: "Run one of this project's owner-approved commands (build, test, or lint) by name. " +
			"The command line comes from the approved manifest and cannot be supplied here. " +
			"A non-zero exit code is a result, not a failure.",
		InputSchema: schemaObject([]string{"name"}, map[string]*jsonschema.Schema{
			"name": schemaString("Which approved command to run: build, test, or lint."),
		}),
	}, func(ctx context.Context, input runCommandInput, timer *toolTimer) (string, error) {
		decision, err := g.resolveCommand(resolved, input.Name)
		if err != nil {
			g.auditRefusal(resolved, input.Name, err)
			return "", err
		}

		names, nameErr := projectstack.DeriveNames(resolved.project.ID)
		networkName := ""
		if nameErr == nil {
			networkName = names.Network
		}

		result, runErr := backendTimed(g, timer, func() (runner.Result, error) {
			return g.options.Runner.Run(ctx, runner.Request{
				ProjectID:   resolved.project.ID,
				Workspace:   resolved.project.Path,
				Image:       decision.Image,
				Command:     decision.Command,
				Network:     decision.Network,
				NetworkName: networkName,
				Limits:      g.runnerLimits(resolved),
			})
		})
		if runErr != nil {
			toolErr := asRunToolError(runErr)
			g.auditRefusal(resolved, input.Name, toolErr)
			return "", toolErr
		}

		g.audit(resolved, input.Name, result)

		var builder strings.Builder
		fmt.Fprintf(&builder, "%s: %s (image %s, network %s)\n", decision.Name, result.Command, result.Image, result.Network)
		verdict := fmt.Sprintf("exit code %d after %.1fs", result.ExitCode, result.Seconds)
		if result.TimedOut {
			verdict += " — timed out"
		}
		builder.WriteString(verdict)
		if stdout := strings.TrimSpace(result.Stdout); stdout != "" {
			builder.WriteString("\n--- stdout ---\n" + stdout)
			if result.StdoutTruncated {
				builder.WriteString("\n[stdout truncated — the tail is kept]")
			}
		}
		if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
			builder.WriteString("\n--- stderr ---\n" + stderr)
			if result.StderrTruncated {
				builder.WriteString("\n[stderr truncated — the tail is kept]")
			}
		}
		return builder.String(), nil
	})
}

// resolveCommand turns a name into something that may run, or a typed refusal.
func (g *Gateway) resolveCommand(resolved serveContext, name string) (commandpolicy.Resolved, error) {
	load := g.options.Manifest
	if load == nil {
		load = projectmanifest.Load
	}
	manifest, err := load(resolved.project.Path)
	if err != nil {
		return commandpolicy.Resolved{}, &searchToolError{
			code:    CodeRunnerFailed,
			message: "The project's manifest could not be read: " + firstLine(err.Error()),
			action:  "Fix the manifest, or check it with lctk project status.",
		}
	}

	decision, err := resolved.project.Commands.Resolve(name, proposalsOf(manifest))
	if err == nil {
		return decision, nil
	}

	// Each refusal names what a person has to do about it, because they are
	// genuinely different acts and an agent relaying one to a user should relay
	// the right one.
	switch {
	case errors.Is(err, commandpolicy.ErrUnknownName):
		return decision, &searchToolError{
			code:    CodeCommandUnknown,
			message: "LCTK runs only build, test, and lint.",
			action:  "Ask for one of those.",
		}
	case errors.Is(err, commandpolicy.ErrNotProposed):
		return decision, &searchToolError{
			code:    CodeCommandNotProposed,
			message: "This project's manifest does not propose a " + name + " command.",
			action:  "Add commands." + name + " to .mcp-project.yaml, then approve it.",
		}
	case errors.Is(err, commandpolicy.ErrNotApproved):
		return decision, &searchToolError{
			code:    CodeCommandNotApproved,
			message: "The " + name + " command has not been approved for this project.",
			action:  "The machine owner approves it with lctk project commands --approve " + name + ".",
		}
	case errors.Is(err, commandpolicy.ErrChanged):
		return decision, &searchToolError{
			code:    CodeCommandChanged,
			message: "The " + name + " command changed since it was approved, so the approval no longer applies.",
			action:  "The machine owner reviews the new command and approves it again.",
		}
	case errors.Is(err, commandpolicy.ErrNoImage):
		return decision, &searchToolError{
			code:    CodeRunnerNotConfigured,
			message: "This project has no runner image, so nothing can be run in it.",
			action:  "The machine owner sets one with lctk project commands --image IMAGE.",
		}
	default:
		return decision, &searchToolError{code: CodeRunnerFailed, message: firstLine(err.Error())}
	}
}

func proposalsOf(manifest projectmanifest.Result) []commandpolicy.Proposal {
	return []commandpolicy.Proposal{
		{Name: commandpolicy.NameBuild, Command: manifest.Manifest.Commands.Build},
		{Name: commandpolicy.NameTest, Command: manifest.Manifest.Commands.Test},
		{Name: commandpolicy.NameLint, Command: manifest.Manifest.Commands.Lint},
	}
}

// runnerLimits gives a command the same background-load budget the project's
// indexing gets, so one setting governs what the project costs.
func (g *Gateway) runnerLimits(resolved serveContext) runner.Limits {
	limits := runner.DefaultLimits
	if g.options.Budget == nil {
		return limits
	}
	budget := g.options.Budget(resolved.project)
	if budget.CPUs > 0 {
		limits.CPUs = budget.CPUs
	}
	if budget.MemoryLimitMB > 0 {
		limits.MemoryMB = budget.MemoryLimitMB
	}
	return limits
}

func (g *Gateway) audit(resolved serveContext, name string, result runner.Result) {
	if g.options.Audit == nil {
		return
	}
	_ = g.options.Audit.Append(auditlog.Entry{
		At:              g.options.Now(),
		ProjectID:       resolved.project.ID,
		Name:            name,
		Command:         result.Command,
		Image:           result.Image,
		Network:         result.Network,
		Client:          resolved.authorization.ClientName,
		AuthorizationID: resolved.authorization.ID,
		ExitCode:        result.ExitCode,
		TimedOut:        result.TimedOut,
		Seconds:         result.Seconds,
		Output:          result.Stdout + result.Stderr,
	})
}

// auditRefusal records a run that never happened. A refusal is as much a part of
// the record as an execution: "the agent kept asking and was kept out" is
// something an operator needs to be able to see.
func (g *Gateway) auditRefusal(resolved serveContext, name string, err error) {
	if g.options.Audit == nil {
		return
	}
	_ = g.options.Audit.Append(auditlog.Entry{
		At:              g.options.Now(),
		ProjectID:       resolved.project.ID,
		Name:            name,
		Client:          resolved.authorization.ClientName,
		AuthorizationID: resolved.authorization.ID,
		ExitCode:        -1,
		Refused:         err.Error(),
	})
}

func asRunToolError(err error) error {
	switch {
	case errors.Is(err, runner.ErrRuntimeUnavailable):
		return &searchToolError{
			code:      CodeRunnerUnavailable,
			message:   "The container runtime is not available.",
			retryable: true,
			action:    "Start or repair the LCTK managed runtime from the Admin UI.",
		}
	case errors.Is(err, runner.ErrImageMissing):
		return &searchToolError{
			code:    CodeRunnerNotConfigured,
			message: "The project's runner image is not available: " + firstLine(err.Error()),
			action:  "Pull the image, or approve one that exists with lctk project commands --image IMAGE.",
		}
	default:
		return &searchToolError{
			code:      CodeRunnerFailed,
			message:   firstLine(err.Error()),
			retryable: true,
		}
	}
}

// runnableNames lists the commands this project can actually run right now, for
// project_info to advertise.
func (g *Gateway) runnableNames(resolved serveContext) []string {
	if g.options.Runner == nil {
		return nil
	}
	load := g.options.Manifest
	if load == nil {
		load = projectmanifest.Load
	}
	manifest, err := load(resolved.project.Path)
	if err != nil {
		return nil
	}

	var names []string
	for _, status := range resolved.project.Commands.Describe(proposalsOf(manifest)) {
		if status.Runnable {
			names = append(names, status.Name)
		}
	}
	return names
}
