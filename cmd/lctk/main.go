package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/daemon"
	"github.com/lev-goryachev/lctk/internal/runtimeapi"
	"github.com/lev-goryachev/lctk/internal/watcher"
)

const usage = `Local Code ToolKit

Usage:
  lctk version [--json]
  lctk daemon [--listen ADDRESS]
  lctk doctor [--json]
  lctk bootstrap [--manifest SOURCE] [--plan] [--yes] [--json]
  lctk update [--manifest SOURCE] [--plan] [--yes] [--json]
  lctk update rollback [--json]
  lctk watch-once [--timeout DURATION] DIRECTORY
  lctk project add [--profile minimal|full] [--json] PATH
  lctk project status [--json] [PROJECT]
  lctk project start [--wait DURATION] [--yes] [--json] PROJECT
  lctk project stop [--json] PROJECT
  lctk project restart [--wait DURATION] [--yes] [--json] PROJECT
  lctk project remove [--json] PROJECT
  lctk project reindex [--full] [--json] PROJECT
  lctk project watch [--follow] [--json] PROJECT
  lctk project resources [--mode quiet|normal|fast|default] [--json] PROJECT
  lctk project commands [--approve NAME] [--image IMAGE] [--json] PROJECT
  lctk settings show [--json]
  lctk admin open
  lctk image build [--context DIR] [--json]
  lctk image status [--json]
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return runDesktop(ctx, stdout)
	}

	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout)
	case "daemon":
		return runDaemon(ctx, args[1:], stdout)
	case "doctor":
		return runDoctor(ctx, args[1:], stdout)
	case "bootstrap":
		return runBootstrap(ctx, args[1:], stdout)
	case "update":
		return runUpdate(ctx, args[1:], stdout)
	case "watch-once":
		return runWatchOnce(ctx, args[1:], stdout)
	case "project":
		return runProject(args[1:], stdout, stderr)
	case "settings":
		return runSettings(args[1:], stdout, stderr)
	case "admin":
		return runAdmin(args[1:], stdout, stderr)
	case "image":
		return runImage(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runVersion(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write version information as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("version does not accept positional arguments")
	}

	info := buildinfo.Current()
	if *asJSON {
		return json.NewEncoder(output).Encode(info)
	}
	fmt.Fprintf(output, "lctk %s (%s/%s, %s)\n", info.Version, info.OS, info.Arch, info.Commit)
	return nil
}

func runDaemon(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	address := flags.String("listen", daemon.DefaultAddress, "loopback listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("daemon does not accept positional arguments")
	}

	fmt.Fprintf(output, "LCTK daemon listening on http://%s\n", *address)
	return daemon.Run(ctx, *address)
}

func runDoctor(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write diagnostics as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("doctor does not accept positional arguments")
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := runtimeapi.Probe(probeCtx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(output).Encode(status)
	}
	fmt.Fprintf(output, "Managed %s runtime available (%s, %s)\n", status.Provider, status.Version, status.OSType)
	return nil
}

func runWatchOnce(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("watch-once", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time to wait")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: lctk watch-once [--timeout DURATION] DIRECTORY")
	}

	watchCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	change, err := watcher.ObserveOnce(watchCtx, flags.Arg(0))
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(change)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "lctk: %v\n", err)
		os.Exit(1)
	}
}
