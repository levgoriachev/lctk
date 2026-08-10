package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/daemonstate"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/updateflow"
)

// These aliases retain the CLI test seam while the transaction itself lives in
// updateflow and is shared with the native Windows installer.
type (
	updateProject   = updateflow.Project
	updatePlan      = updateflow.Plan
	updateStack     = updateflow.Stack
	updateInstaller = updateflow.Installer
)

var (
	// Manifest verification belongs to the command surface: updateflow receives
	// only an already authenticated immutable component contract.
	newUpdateVerifier  = releasebundle.ProductionVerifier
	loadUpdateManifest = func(ctx context.Context, source string, verifier releasebundle.Verifier) (releasebundle.Manifest, error) {
		return releasebundle.Load(ctx, source, http.DefaultClient, verifier)
	}
	newUpdateStack = func(version string, distribution inference.Distribution) updateStack {
		return projectstack.NewManager().WithVersion(version).WithInferenceDistribution(distribution)
	}
	newUpdateInstaller = func(home string) updateInstaller {
		return installation.NewManager(home)
	}
	loadUpdateRegistry = projectregistry.Load
	loadUpdateDaemon   = daemonstate.Load
	stopUpdateDaemon   = daemonstate.Stop
	startUpdateDaemon  = daemonstate.Start
	newUpdateInference = func(distribution inference.Distribution) (updateflow.Inference, error) {
		return inference.NewManagerForDistribution(containerruntime.Runner{}, distribution)
	}
)

// runUpdate presents the CLI's plan/apply contract while delegating every
// lifecycle mutation and rollback rule to the shared coordinator.
func runUpdate(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "rollback" {
		return runUpdateRollback(ctx, args[1:], stdout)
	}
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	source := flags.String("manifest", "", "signed release manifest HTTPS URL or local file")
	planOnly := flags.Bool("plan", false, "perform read-only update preflight")
	proceed := flags.Bool("yes", false, "apply the verified update plan")
	asJSON := flags.Bool("json", false, "write machine-readable plan and result")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: lctk update [--manifest SOURCE] [--plan] [--yes] [--json]")
	}

	verifier, err := newUpdateVerifier()
	if err != nil {
		return err
	}
	manifest, err := loadUpdateManifest(ctx, *source, verifier)
	if err != nil {
		return err
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	manager := updateManager(home, buildinfo.Version, *source)
	if *planOnly || !*proceed {
		preflightCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		plan, err := manager.Inspect(preflightCtx, manifest)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(stdout, plan)
		}
		running := 0
		for _, project := range plan.Projects {
			if project.Running {
				running++
			}
		}
		fmt.Fprintf(stdout, "Verified update %s -> %s; %d registered projects, %d running; writes: false\n",
			plan.CurrentVersion, plan.TargetVersion, len(plan.Projects), running)
		if !*planOnly {
			fmt.Fprintln(stdout, "No changes applied. Review the plan, then run lctk update --yes.")
		}
		return nil
	}

	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	restartDaemon, err := suspendUpdateDaemon(home)
	if err != nil {
		return err
	}
	plan, applyErr := manager.Apply(applyCtx, manifest)
	restartErr := restartDaemon()
	if applyErr != nil || restartErr != nil {
		return errors.Join(applyErr, restartErr)
	}
	if *asJSON {
		return writeJSON(stdout, plan)
	}
	fmt.Fprintf(stdout, "Updated to %s; every previously running project passed its health gate.\n", manifest.Version)
	return nil
}

// runUpdateRollback retains the public CLI command while using the same
// verified rollback sequence available to setup after a later phase fails.
func runUpdateRollback(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("update rollback", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write machine-readable rollback result")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: lctk update rollback [--json]")
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	rollbackCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	restartDaemon, err := suspendUpdateDaemon(home)
	if err != nil {
		return err
	}
	plan, rollbackErr := updateManager(home, buildinfo.Version, "").Rollback(rollbackCtx)
	restartErr := restartDaemon()
	if rollbackErr != nil || restartErr != nil {
		return errors.Join(rollbackErr, restartErr)
	}
	if *asJSON {
		return writeJSON(stdout, plan)
	}
	fmt.Fprintf(stdout, "Rolled back to %s.\n", plan.TargetVersion)
	return nil
}

// suspendUpdateDaemon keeps the long-lived host on the same activation as the
// project and inference transaction. Setup already owns this boundary around
// updateflow directly; the CLI must establish it itself before changing the
// active core. A missing state means no daemon was running and must not create
// one as an update side effect.
func suspendUpdateDaemon(home string) (func() error, error) {
	noRestart := func() error { return nil }
	if _, err := loadUpdateDaemon(home); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return noRestart, nil
		}
		return nil, fmt.Errorf("inspect installed LCTK background service: %w", err)
	}
	if err := stopUpdateDaemon(home); err != nil {
		return nil, fmt.Errorf("stop installed LCTK background service: %w", err)
	}
	return func() error {
		if err := startUpdateDaemon(home); err != nil {
			return fmt.Errorf("restart installed LCTK background service: %w", err)
		}
		return nil
	}, nil
}

// updateManager binds command-level injectable factories to the shared
// coordinator. Production and setup use the same transaction implementation.
func updateManager(home, currentVersion, manifestSource string) *updateflow.Manager {
	manager := updateflow.NewManager(home, currentVersion, manifestSource)
	manager.Installer = newUpdateInstaller(home)
	manager.LoadRegistry = loadUpdateRegistry
	manager.NewStack = newUpdateStack
	manager.NewInference = newUpdateInference
	return manager
}

// rollbackRegisteredProjects preserves the focused legacy unit-test entry point
// while delegating its behavior to the shared coordinator.
func rollbackRegisteredProjects(ctx context.Context, current, previous updateStack,
	projects, running []projectregistry.Project, previousVersion string) error {
	return updateflow.RollbackRegisteredProjects(ctx, current, previous, projects, running, previousVersion, updateflow.DefaultStartWait)
}
