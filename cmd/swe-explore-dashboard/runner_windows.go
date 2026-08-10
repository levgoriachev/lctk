//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/lev-goryachev/lctk/internal/sweexplore"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
	"golang.org/x/sys/windows"
)

type dashboardRunMode string

const (
	runPrepareNext  dashboardRunMode = "prepare-next"
	runNextInstance dashboardRunMode = "run-next"
	runAllRemaining dashboardRunMode = "run-all"
)

// ownedCampaignProcess binds the runner and every child client to a Windows
// job. Stop and dashboard shutdown therefore terminate only the exact process
// tree created by this UI, never an ambient Codex, Claude, or LCTK process.
type ownedCampaignProcess struct {
	command *exec.Cmd
	job     windows.Handle
	stdout  *os.File
	stderr  *os.File
	once    sync.Once
}

func startCampaignProcess(options dashboardOptions, snapshot sweexplore.DashboardSnapshot, mode dashboardRunMode) (*ownedCampaignProcess, error) {
	if !options.AllowRun {
		return nil, fmt.Errorf("campaign controls are disabled; restart the dashboard with --allow-run")
	}
	if snapshot.FirstIncompleteOrdinal == 0 {
		return nil, fmt.Errorf("campaign is already complete")
	}
	manifest, err := loadRunnerManifest(options.ManifestPath)
	if err != nil {
		return nil, err
	}
	if err := validateRunnerCheckout(options.RunnerWorkdir, manifest.HarnessCommit); err != nil {
		return nil, err
	}
	running, err := exactHarnessRunning(options.HarnessPath)
	if err != nil {
		return nil, err
	}
	if running {
		return nil, fmt.Errorf("the exact campaign harness is already running")
	}
	arguments := []string{"campaign", "--config", options.ConfigPath, "--manifest", options.ManifestPath, "--output-dir", options.OutputRoot, "--python", options.Python}
	switch mode {
	case runPrepareNext:
		arguments = append(arguments, "--max-instances", fmt.Sprint(snapshot.FirstIncompleteOrdinal), "--prepare-only")
	case runNextInstance:
		arguments = append(arguments, "--max-instances", fmt.Sprint(snapshot.FirstIncompleteOrdinal))
	case runAllRemaining:
	default:
		return nil, fmt.Errorf("unsupported dashboard run mode %q", mode)
	}
	logRoot := filepath.Join(options.OutputRoot, "dashboard-runs", time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+string(mode))
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create dashboard run log directory: %w", err)
	}
	stdout, err := os.OpenFile(filepath.Join(logRoot, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(logRoot, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	command := exec.Command(options.HarnessPath, arguments...)
	command.Dir = options.RunnerWorkdir
	command.Stdout = stdout
	command.Stderr = stderr
	windowsprocess.HideConsole(command)
	job, err := createCampaignJob()
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	process := &ownedCampaignProcess{command: command, job: job, stdout: stdout, stderr: stderr}
	if err := command.Start(); err != nil {
		process.close(false)
		return nil, fmt.Errorf("start campaign harness: %w", err)
	}
	processHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		_ = command.Process.Kill()
		process.close(false)
		return nil, fmt.Errorf("open campaign process for lifecycle ownership: %w", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, processHandle)
	windows.CloseHandle(processHandle)
	if assignErr != nil {
		_ = command.Process.Kill()
		process.close(false)
		return nil, fmt.Errorf("assign campaign process tree: %w", assignErr)
	}
	return process, nil
}

func (process *ownedCampaignProcess) wait() error {
	err := process.command.Wait()
	process.close(false)
	return err
}

func (process *ownedCampaignProcess) stop() {
	process.close(true)
}

func (process *ownedCampaignProcess) close(kill bool) {
	process.once.Do(func() {
		if kill && process.job != 0 {
			windows.CloseHandle(process.job)
			process.job = 0
			_ = process.command.Process.Kill()
		} else if process.job != 0 {
			windows.CloseHandle(process.job)
			process.job = 0
		}
		_ = process.stdout.Close()
		_ = process.stderr.Close()
	})
}

func createCampaignJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create campaign process job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("configure campaign process job: %w", err)
	}
	return job, nil
}

func loadRunnerManifest(path string) (sweexplore.CampaignManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return sweexplore.CampaignManifest{}, err
	}
	var manifest sweexplore.CampaignManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return sweexplore.CampaignManifest{}, err
	}
	return manifest, nil
}

func validateRunnerCheckout(workdir, expectedCommit string) error {
	command := exec.Command("git", "-C", workdir, "rev-parse", "HEAD")
	windowsprocess.HideConsole(command)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect runner worktree: %w", err)
	}
	actual := strings.TrimSpace(string(output))
	if !strings.EqualFold(actual, expectedCommit) {
		return fmt.Errorf("runner worktree is at %s, manifest pins %s", actual, expectedCommit)
	}
	return nil
}

func exactHarnessRunning(target string) (bool, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, fmt.Errorf("enumerate campaign processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), filepath.Base(target)) {
			continue
		}
		handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
		if openErr != nil {
			continue
		}
		buffer := make([]uint16, 32768)
		size := uint32(len(buffer))
		queryErr := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size)
		windows.CloseHandle(handle)
		if queryErr == nil && strings.EqualFold(filepath.Clean(windows.UTF16ToString(buffer[:size])), filepath.Clean(target)) {
			return true, nil
		}
	}
	if err != nil && err != windows.ERROR_NO_MORE_FILES {
		return false, fmt.Errorf("enumerate campaign processes: %w", err)
	}
	return false, nil
}
