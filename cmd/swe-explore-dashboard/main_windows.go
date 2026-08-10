//go:build windows

// Command swe-explore-dashboard provides a small native control and monitoring
// surface over immutable SWE-Explore campaign artifacts. It is development
// tooling and is not included in the LCTK installer.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lev-goryachev/lctk/internal/sweexplore"
)

// dashboardOptions keeps every read and launch path explicit. The dashboard
// never discovers an ambient benchmark, harness, Python, or Git checkout.
type dashboardOptions struct {
	ConfigPath    string
	ManifestPath  string
	HarnessPath   string
	OutputRoot    string
	RunnerWorkdir string
	Python        string
	AllowRun      bool
	SnapshotJSON  bool
}

func main() {
	options, err := parseDashboardOptions(os.Args[1:])
	if err == nil && options.SnapshotJSON {
		var snapshot sweexplore.DashboardSnapshot
		snapshot, err = sweexplore.LoadDashboardSnapshot(options.ConfigPath, options.ManifestPath, options.HarnessPath, options.OutputRoot)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(snapshot)
		}
	} else if err == nil {
		err = runDashboardWindow(options)
	}
	if err != nil {
		showDashboardError(0, "SWE-Explore Dashboard", err.Error())
		os.Exit(1)
	}
}

func parseDashboardOptions(args []string) (dashboardOptions, error) {
	flags := flag.NewFlagSet("swe-explore-dashboard", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	config := flags.String("config", "", "immutable campaign configuration")
	manifest := flags.String("manifest", "", "immutable campaign manifest")
	harness := flags.String("harness", "", "exact campaign harness executable")
	output := flags.String("output-dir", "", "campaign artifact root")
	workdir := flags.String("runner-workdir", "", "Git checkout pinned to the harness commit")
	python := flags.String("python", "python", "Python executable for official scoring")
	allowRun := flags.Bool("allow-run", false, "enable paid and GPU-mutating campaign controls")
	snapshotJSON := flags.Bool("snapshot-json", false, "print one validated dashboard snapshot instead of opening the window")
	if err := flags.Parse(args); err != nil {
		return dashboardOptions{}, err
	}
	if flags.NArg() != 0 || *config == "" || *manifest == "" || *harness == "" || *output == "" {
		return dashboardOptions{}, fmt.Errorf("usage: swe-explore-dashboard --config FILE --manifest FILE --harness EXE --output-dir DIR [--runner-workdir DIR] [--python EXE] [--allow-run]")
	}
	if *allowRun && *workdir == "" {
		return dashboardOptions{}, fmt.Errorf("--runner-workdir is required with --allow-run")
	}
	resolved := dashboardOptions{Python: *python, AllowRun: *allowRun, SnapshotJSON: *snapshotJSON}
	var err error
	for source, destination := range map[*string]*string{
		config: &resolved.ConfigPath, manifest: &resolved.ManifestPath, harness: &resolved.HarnessPath,
		output: &resolved.OutputRoot, workdir: &resolved.RunnerWorkdir,
	} {
		if *source == "" {
			continue
		}
		*destination, err = filepath.Abs(*source)
		if err != nil {
			return dashboardOptions{}, err
		}
	}
	for label, path := range map[string]string{"config": resolved.ConfigPath, "manifest": resolved.ManifestPath, "harness": resolved.HarnessPath} {
		if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
			return dashboardOptions{}, fmt.Errorf("%s file is unavailable: %s", label, path)
		}
	}
	if resolved.RunnerWorkdir != "" {
		if info, statErr := os.Stat(resolved.RunnerWorkdir); statErr != nil || !info.IsDir() {
			return dashboardOptions{}, fmt.Errorf("runner worktree is unavailable: %s", resolved.RunnerWorkdir)
		}
	}
	return resolved, nil
}
