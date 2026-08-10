// lctk-setup is the native Windows bootstrapper. It keeps release verification,
// the read-only plan, path selection, progress, and mutation in one process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/daemon"
	"github.com/lev-goryachev/lctk/internal/diskspace"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/setupflow"
	"github.com/lev-goryachev/lctk/internal/uninstall"
	"github.com/lev-goryachev/lctk/internal/windowssetup"
)

// A fresh WSL machine uses a sparse 60 GiB virtual disk, so setup does not
// require the full logical maximum up front. Four GiB covers the imported base,
// both initial OCI images, and an operational margin before the first project.
const freshRuntimeDataMinimum = 4 << 30

type setupRequest struct {
	Context        context.Context
	Manifest       releasebundle.Manifest
	ManifestSource string
	Host           windowssetup.Status
	Locations      lctkhome.Locations
	Action         setupflow.Action
	CurrentVersion string
	InstallLocked  bool
	RuntimeLocked  bool
	Distribution   inference.Distribution
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		showError(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("lctk-setup", flag.ContinueOnError)
	manifestSource := flags.String("manifest", "", "signed release manifest HTTPS URL")
	resume := flags.Bool("resume", false, "resume after Windows restart")
	uninstallRequested := flags.Bool("uninstall", false, "remove LCTK and its managed runtime")
	adminRequested := flags.Bool("admin", false, "open the native LCTK administrator window")
	adminAddress := flags.String("listen", daemon.DefaultAddress, "loopback daemon address used by --admin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("setup does not accept positional arguments")
	}
	selectedModes := 0
	for _, selected := range []bool{*resume, *uninstallRequested, *adminRequested} {
		if selected {
			selectedModes++
		}
	}
	if selectedModes > 1 {
		return errors.New("setup modes --resume, --uninstall, and --admin are mutually exclusive")
	}
	if !*adminRequested && *adminAddress != daemon.DefaultAddress {
		return errors.New("--listen is valid only together with --admin")
	}
	_ = resume
	if *adminRequested {
		return runAdminWindow(ctx, *adminAddress)
	}
	if *uninstallRequested {
		return runUninstall(ctx)
	}
	verifier, err := releasebundle.ProductionVerifier()
	if err != nil {
		return err
	}
	// Setup has no window until the signed identity is known. Bound this small
	// metadata request so an unreachable release host produces a visible error
	// instead of leaving a background process with no interface indefinitely.
	manifestCtx, cancelManifest := context.WithTimeout(ctx, 30*time.Second)
	defer cancelManifest()
	manifest, err := releasebundle.Load(manifestCtx, *manifestSource, http.DefaultClient, verifier)
	if err != nil {
		return err
	}
	host, err := windowssetup.Probe(ctx)
	if err != nil {
		return err
	}
	if host.RequiresEnablement && !host.Elevated {
		arguments := []string{"--resume"}
		if *manifestSource != "" {
			arguments = append(arguments, "--manifest", *manifestSource)
		}
		return windowssetup.RelaunchElevated(arguments)
	}
	locations, err := lctkhome.CurrentLocations()
	if err != nil {
		return err
	}
	activation, activationErr := installation.Load(locations.InstallDir)
	installLocked := activationErr == nil
	if activationErr != nil && !errors.Is(activationErr, os.ErrNotExist) {
		return activationErr
	}
	currentVersion := ""
	if installLocked {
		currentVersion = activation.ActiveVersion
	}
	action, err := setupflow.DecideAction(currentVersion, manifest.Version)
	if err != nil {
		return err
	}
	machineCtx, cancelMachine := context.WithTimeout(ctx, 15*time.Second)
	defer cancelMachine()
	runtimeLocked, err := containerruntime.MachineExists(machineCtx)
	if err != nil {
		return err
	}
	selection, err := inference.LoadSelection()
	if err != nil {
		return err
	}
	return runSetupWindow(setupRequest{
		Context: ctx, Manifest: manifest, ManifestSource: *manifestSource,
		Host: host, Locations: locations, Action: action, CurrentVersion: currentVersion,
		InstallLocked: installLocked, RuntimeLocked: runtimeLocked,
		Distribution: selection.Distribution,
	})
}

func runUninstall(ctx context.Context) error {
	locations, err := lctkhome.CurrentLocations()
	if err != nil {
		return err
	}
	preserve, proceed := confirmUninstall(locations)
	if !proceed {
		return nil
	}
	home := locations.InstallDir
	backup, err := uninstall.NewManager(home).Run(ctx, preserve)
	if err != nil {
		return err
	}
	if err := lctkhome.ClearLocations(); err != nil {
		return err
	}
	message := "LCTK and its managed runtime were removed."
	if backup != "" {
		message += " Project state archives were preserved in " + backup + "."
	}
	showInfo(message)
	return nil
}

// inspectSelection applies the selected locations to this process before any
// Podman or host-core path is resolved, then recalculates the complete plan.
func inspectSelection(ctx context.Context, request setupRequest, locations lctkhome.Locations) (*setupflow.Manager, setupflow.Plan, error) {
	if err := lctkhome.ConfigureProcess(locations); err != nil {
		return nil, setupflow.Plan{}, err
	}
	manager := setupflow.NewManager(locations.InstallDir, request.ManifestSource)
	manager.Distribution = request.Distribution
	plan, err := manager.Inspect(ctx, request.Manifest)
	if err != nil {
		return nil, setupflow.Plan{}, err
	}
	if plan.Action != request.Action || plan.CurrentVersion != request.CurrentVersion {
		return nil, setupflow.Plan{}, errors.New("the installed LCTK version changed while setup was open; reopen setup to review the new plan")
	}
	available, err := diskspace.Available(locations.RuntimeDataDir)
	if err != nil {
		return nil, setupflow.Plan{}, fmt.Errorf("inspect selected runtime-data volume: %w", err)
	}
	plan.RuntimeDataAvailableBytes = available
	minimum := plan.InferenceRuntimeBytes
	if !request.RuntimeLocked {
		minimum += int64(freshRuntimeDataMinimum)
	}
	plan.RuntimeDataRequiredBytes = minimum
	plan.Ready = plan.Ready && available >= uint64(minimum)
	return manager, plan, nil
}

// applySelection persists the accepted layout and executes the exact plan
// through the existing transactional setup coordinator.
func applySelection(ctx context.Context, request setupRequest, locations lctkhome.Locations, progress setupflow.Progress) error {
	manager, plan, err := inspectSelection(ctx, request, locations)
	if err != nil {
		return err
	}
	if !plan.Ready {
		return errors.New("setup plan is not ready for the selected locations")
	}
	if err := lctkhome.SaveLocations(locations); err != nil {
		return err
	}
	if err := releaseInstalledAdminExecutable(locations.InstallDir); err != nil {
		return err
	}
	manager.Progress = progress
	installCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	if err := manager.Install(installCtx, request.Manifest); err != nil {
		return err
	}
	setup := filepath.Join(locations.InstallDir, "bin", "lctk-setup.exe")
	if err := exec.Command(setup, "--admin").Start(); err != nil {
		return fmt.Errorf("open installed LCTK interface: %w", err)
	}
	return nil
}
