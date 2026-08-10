//go:build windows

package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/lev-goryachev/lctk/internal/sweexplore"
	"golang.org/x/sys/windows"
)

const (
	dashboardWidth  = 1180
	dashboardHeight = 890

	wsOverlappedWindow  = 0x00CF0000
	wsChild             = 0x40000000
	wsVisible           = 0x10000000
	wsTabStop           = 0x00010000
	wsBorder            = 0x00800000
	wsVScroll           = 0x00200000
	esMultiline         = 0x0004
	esAutoVScroll       = 0x0040
	esReadOnly          = 0x0800
	bsPushButton        = 0x00000000
	bsGroupBox          = 0x00000007
	ssLeft              = 0x00000000
	lbsNoIntegralHeight = 0x0100

	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmSetFont     = 0x0030
	wmUser        = 0x0400
	wmAppSnapshot = 0x8001

	pbmSetPos      = wmUser + 2
	pbmSetRange32  = wmUser + 6
	lbResetContent = 0x0184
	lbAddString    = 0x0180

	swShow         = 5
	colorWindow    = 5
	idArrow        = 32512
	defaultGUIFont = 17
	ansiFixedFont  = 11

	idPrepareNext = 3001
	idRunNext     = 3002
	idRunAll      = 3003
	idStop        = 3004
	idRefresh     = 3005
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	gdi32                  = windows.NewLazySystemDLL("gdi32.dll")
	comctl32               = windows.NewLazySystemDLL("comctl32.dll")
	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procShowWindow         = user32.NewProc("ShowWindow")
	procUpdateWindow       = user32.NewProc("UpdateWindow")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procSendMessageW       = user32.NewProc("SendMessageW")
	procSetWindowTextW     = user32.NewProc("SetWindowTextW")
	procEnableWindow       = user32.NewProc("EnableWindow")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procLoadCursorW        = user32.NewProc("LoadCursorW")
	procMessageBoxW        = user32.NewProc("MessageBoxW")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procGetStockObject     = gdi32.NewProc("GetStockObject")
	procInitCommonControls = comctl32.NewProc("InitCommonControlsEx")

	dashboardWindowClass = mustDashboardUTF16("LCTKSWEExploreDashboard")
	dashboardWindows     sync.Map
)

type dashboardPoint struct {
	X int32
	Y int32
}

type dashboardMessage struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   dashboardPoint
	Private uint32
}

type dashboardWindowClassEx struct {
	Size        uint32
	Style       uint32
	WindowProc  uintptr
	ClassExtra  int32
	WindowExtra int32
	Instance    uintptr
	Icon        uintptr
	Cursor      uintptr
	Background  uintptr
	MenuName    *uint16
	ClassName   *uint16
	SmallIcon   uintptr
}

type dashboardCommonControls struct {
	Size uint32
	ICC  uint32
}

// nativeDashboardWindow owns only presentation state and an optional runner
// process tree started through its own confirmed controls. Every refresh is a
// read-only reconstruction from primary campaign evidence.
type nativeDashboardWindow struct {
	options dashboardOptions
	window  uintptr
	context context.Context
	cancel  context.CancelFunc
	refresh chan struct{}

	headerLabel       uintptr
	modeLabel         uintptr
	armsLabel         uintptr
	armsProgress      uintptr
	pairsLabel        uintptr
	pairsProgress     uintptr
	instancesLabel    uintptr
	instancesProgress uintptr
	failuresLabel     uintptr
	activityText      uintptr
	activityProgress  uintptr
	gpuText           uintptr
	resultsText       uintptr
	instanceList      uintptr
	footerLabel       uintptr
	buttons           map[uint16]uintptr

	mu              sync.RWMutex
	snapshot        sweexplore.DashboardSnapshot
	loadError       string
	runner          *ownedCampaignProcess
	runnerStatus    string
	externalRunning bool
}

func runDashboardWindow(options dashboardOptions) error {
	window, err := newNativeDashboardWindow(options)
	if err != nil {
		return err
	}
	defer window.cancel()
	window.refreshNow()
	go window.refreshLoop()
	procShowWindow.Call(window.window, swShow)
	procUpdateWindow.Call(window.window)
	var current dashboardMessage
	for {
		result, _, callErr := procGetMessageW.Call(uintptr(unsafe.Pointer(&current)), 0, 0, 0)
		if int32(result) == -1 {
			return fmt.Errorf("read dashboard window message: %w", callErr)
		}
		if result == 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&current)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&current)))
	}
}

func newNativeDashboardWindow(options dashboardOptions) (*nativeDashboardWindow, error) {
	instance, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idArrow)
	class := dashboardWindowClassEx{Size: uint32(unsafe.Sizeof(dashboardWindowClassEx{})), WindowProc: windows.NewCallback(dashboardWindowProc), Instance: instance, Cursor: cursor, Background: colorWindow + 1, ClassName: dashboardWindowClass}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class)))
	controls := dashboardCommonControls{Size: uint32(unsafe.Sizeof(dashboardCommonControls{})), ICC: 0x20}
	procInitCommonControls.Call(uintptr(unsafe.Pointer(&controls)))
	screenWidth, _, _ := procGetSystemMetrics.Call(0)
	screenHeight, _, _ := procGetSystemMetrics.Call(1)
	x := (int32(screenWidth) - dashboardWidth) / 2
	y := (int32(screenHeight) - dashboardHeight) / 2
	handle, _, callErr := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(dashboardWindowClass)), uintptr(unsafe.Pointer(mustDashboardUTF16("SWE-Explore Benchmark"))), wsOverlappedWindow, uintptr(x), uintptr(y), dashboardWidth, dashboardHeight, 0, 0, instance, 0)
	if handle == 0 {
		return nil, fmt.Errorf("create benchmark dashboard window: %w", callErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	window := &nativeDashboardWindow{options: options, window: handle, context: ctx, cancel: cancel, refresh: make(chan struct{}, 1), buttons: map[uint16]uintptr{}, runnerStatus: "Idle"}
	dashboardWindows.Store(handle, window)
	if err := window.createControls(instance); err != nil {
		dashboardWindows.Delete(handle)
		procDestroyWindow.Call(handle)
		cancel()
		return nil, err
	}
	return window, nil
}

func (window *nativeDashboardWindow) createControls(instance uintptr) error {
	font, _, _ := procGetStockObject.Call(defaultGUIFont)
	fixedFont, _, _ := procGetStockObject.Call(ansiFixedFont)
	create := func(className, text string, style uint32, x, y, width, height int32, id uintptr, selectedFont uintptr) (uintptr, error) {
		handle, _, callErr := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(mustDashboardUTF16(className))), uintptr(unsafe.Pointer(mustDashboardUTF16(text))), uintptr(style), uintptr(x), uintptr(y), uintptr(width), uintptr(height), window.window, id, instance, 0)
		if handle == 0 {
			return 0, fmt.Errorf("create %s dashboard control: %w", className, callErr)
		}
		if selectedFont == 0 {
			selectedFont = font
		}
		procSendMessageW.Call(handle, wmSetFont, selectedFont, 1)
		return handle, nil
	}
	label := func(text string, x, y, width, height int32) (uintptr, error) {
		return create("STATIC", text, wsChild|wsVisible|ssLeft, x, y, width, height, 0, font)
	}
	var err error
	window.headerLabel, err = label("SWE-Explore benchmark", 24, 16, 520, 26)
	if err != nil {
		return err
	}
	window.modeLabel, err = label("", 24, 45, 700, 22)
	if err != nil {
		return err
	}
	buttonSpecs := []struct {
		id       uintptr
		text     string
		x, width int32
	}{
		{idPrepareNext, "Prepare next", 628, 112}, {idRunNext, "Run next", 748, 100},
		{idRunAll, "Run remaining", 856, 124}, {idStop, "Stop", 988, 72}, {idRefresh, "Refresh", 1068, 82},
	}
	for _, spec := range buttonSpecs {
		button, createErr := create("BUTTON", spec.text, wsChild|wsVisible|wsTabStop|bsPushButton, spec.x, 18, spec.width, 30, spec.id, font)
		if createErr != nil {
			return createErr
		}
		window.buttons[uint16(spec.id)] = button
	}
	if _, err = create("BUTTON", "Campaign", wsChild|wsVisible|bsGroupBox, 24, 76, 1126, 112, 0, font); err != nil {
		return err
	}
	window.armsLabel, err = label("Arms", 42, 101, 315, 20)
	if err != nil {
		return err
	}
	window.armsProgress, err = create("msctls_progress32", "", wsChild|wsVisible, 42, 126, 315, 18, 0, font)
	if err != nil {
		return err
	}
	window.pairsLabel, err = label("Pairs", 390, 101, 315, 20)
	if err != nil {
		return err
	}
	window.pairsProgress, err = create("msctls_progress32", "", wsChild|wsVisible, 390, 126, 315, 18, 0, font)
	if err != nil {
		return err
	}
	window.instancesLabel, err = label("Instances", 738, 101, 275, 20)
	if err != nil {
		return err
	}
	window.instancesProgress, err = create("msctls_progress32", "", wsChild|wsVisible, 738, 126, 275, 18, 0, font)
	if err != nil {
		return err
	}
	window.failuresLabel, err = label("Failures", 1032, 101, 100, 48)
	if err != nil {
		return err
	}
	if _, err = create("BUTTON", "Current activity", wsChild|wsVisible|bsGroupBox, 24, 196, 1126, 142, 0, font); err != nil {
		return err
	}
	window.activityText, err = create("EDIT", "", wsChild|wsVisible|wsBorder|esMultiline|esReadOnly, 42, 220, 680, 72, 0, font)
	if err != nil {
		return err
	}
	window.activityProgress, err = create("msctls_progress32", "", wsChild|wsVisible, 42, 301, 680, 18, 0, font)
	if err != nil {
		return err
	}
	window.gpuText, err = create("EDIT", "", wsChild|wsVisible|wsBorder|esMultiline|esReadOnly, 738, 220, 394, 99, 0, font)
	if err != nil {
		return err
	}
	if _, err = create("BUTTON", "Results from validated receipts", wsChild|wsVisible|bsGroupBox, 24, 346, 1126, 226, 0, font); err != nil {
		return err
	}
	window.resultsText, err = create("EDIT", "", wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll|esReadOnly, 42, 372, 1090, 181, 0, fixedFont)
	if err != nil {
		return err
	}
	if _, err = create("BUTTON", "Instances", wsChild|wsVisible|bsGroupBox, 24, 580, 1126, 226, 0, font); err != nil {
		return err
	}
	window.instanceList, err = create("LISTBOX", "", wsChild|wsVisible|wsBorder|wsVScroll|lbsNoIntegralHeight, 42, 606, 1090, 181, 0, fixedFont)
	if err != nil {
		return err
	}
	window.footerLabel, err = label("Loading immutable campaign evidence...", 24, 816, 1126, 38)
	return err
}

func (window *nativeDashboardWindow) refreshLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-window.context.Done():
			return
		case <-ticker.C:
			window.refreshNow()
		case <-window.refresh:
			window.refreshNow()
		}
	}
}

func (window *nativeDashboardWindow) refreshNow() {
	snapshot, loadErr := sweexplore.LoadDashboardSnapshot(window.options.ConfigPath, window.options.ManifestPath, window.options.HarnessPath, window.options.OutputRoot)
	external, processErr := exactHarnessRunning(window.options.HarnessPath)
	window.mu.Lock()
	if loadErr == nil {
		window.snapshot = snapshot
		window.loadError = ""
	} else {
		window.loadError = loadErr.Error()
	}
	if processErr == nil {
		window.externalRunning = external && window.runner == nil
	}
	window.mu.Unlock()
	procPostMessageW.Call(window.window, wmAppSnapshot, 0, 0)
}

func (window *nativeDashboardWindow) render() {
	window.mu.RLock()
	snapshot := window.snapshot
	loadError := window.loadError
	runner := window.runner
	runnerStatus := window.runnerStatus
	external := window.externalRunning
	window.mu.RUnlock()
	mode := "READ-ONLY • runner and GPU controls disabled"
	if window.options.AllowRun {
		mode = "CONTROL ENABLED • paid/GPU actions require confirmation"
	}
	setDashboardText(window.headerLabel, fmt.Sprintf("SWE-Explore • %s", valueOr(snapshot.CampaignID, "loading")))
	setDashboardText(window.modeLabel, mode)
	setDashboardProgress(window.armsProgress, snapshot.CompletedArms, snapshot.TargetArms)
	setDashboardText(window.armsLabel, fmt.Sprintf("Arms  %d / %d", snapshot.CompletedArms, snapshot.TargetArms))
	setDashboardProgress(window.pairsProgress, snapshot.CompletedPairs, snapshot.TargetPairs)
	setDashboardText(window.pairsLabel, fmt.Sprintf("Pairs  %d / %d", snapshot.CompletedPairs, snapshot.TargetPairs))
	setDashboardProgress(window.instancesProgress, snapshot.CompletedInstances, snapshot.RequestedInstances)
	setDashboardText(window.instancesLabel, fmt.Sprintf("Instances  %d / %d  • prepared %d", snapshot.CompletedInstances, snapshot.RequestedInstances, snapshot.PreparedInstances))
	setDashboardText(window.failuresLabel, fmt.Sprintf("Failures\r\n%d", snapshot.FailedAttempts))
	setDashboardText(window.activityText, dashboardActivityText(snapshot))
	activityProgress := int(math.Round(snapshot.Activity.CompletionPercent))
	if snapshot.Activity.ChunksTotal == 0 && snapshot.Activity.ChunksCompleted > 0 {
		activityProgress = 100
	}
	setDashboardProgress(window.activityProgress, activityProgress, 100)
	setDashboardText(window.gpuText, dashboardGPUText(snapshot))
	setDashboardText(window.resultsText, dashboardResultsText(snapshot))
	procSendMessageW.Call(window.instanceList, lbResetContent, 0, 0)
	for _, instance := range snapshot.Instances {
		line := fmt.Sprintf("%02d  %-36s  %-10s  arms %d/4  pairs %d/2", instance.Ordinal, instance.InstanceID, instance.Status, instance.CompletedArms, instance.CompletedPairs)
		pointer := mustDashboardUTF16(line)
		procSendMessageW.Call(window.instanceList, lbAddString, 0, uintptr(unsafe.Pointer(pointer)))
	}
	footer := runnerStatus
	if external {
		footer = "External runner detected • this window remains read-only toward that process"
	}
	if loadError != "" {
		footer = "EVIDENCE ERROR • " + loadError
	} else if snapshot.LastFailure != nil {
		footer += fmt.Sprintf(" • last failure: %s/%s — %s", snapshot.LastFailure.InstanceID, snapshot.LastFailure.Stage, snapshot.LastFailure.Error)
	}
	setDashboardText(window.footerLabel, footer)
	incomplete := snapshot.FirstIncompleteOrdinal > 0
	setDashboardEnabled(window.buttons[idPrepareNext], window.options.AllowRun && runner == nil && !external && incomplete)
	setDashboardEnabled(window.buttons[idRunNext], window.options.AllowRun && runner == nil && !external && incomplete)
	setDashboardEnabled(window.buttons[idRunAll], window.options.AllowRun && runner == nil && !external && incomplete)
	setDashboardEnabled(window.buttons[idStop], runner != nil)
}

func dashboardActivityText(snapshot sweexplore.DashboardSnapshot) string {
	activity := snapshot.Activity
	if activity.InstanceID == "" {
		if snapshot.FirstIncompleteOrdinal > 0 && snapshot.FirstIncompleteOrdinal <= len(snapshot.Instances) {
			next := snapshot.Instances[snapshot.FirstIncompleteOrdinal-1]
			return fmt.Sprintf("Next: %s (%s)\r\nNo durable activity has been recorded yet.", next.InstanceID, next.Repository)
		}
		return "Campaign complete. No active preparation or agent trace."
	}
	updated := dashboardAge(activity.UpdatedAt)
	if activity.Stage == "agent" {
		return fmt.Sprintf("%s • %s\r\nArm %s • trace %s bytes • updated %s", activity.InstanceID, activity.Repository, activity.ArmID, formatDashboardInt(activity.TraceBytes), updated)
	}
	progress := fmt.Sprintf("%s committed chunks", formatDashboardInt(int64(activity.ChunksCompleted)))
	if activity.ChunksTotal > 0 {
		progress = fmt.Sprintf("%s / %s chunks • %.1f%% • %.2f chunks/s", formatDashboardInt(int64(activity.ChunksCompleted)), formatDashboardInt(int64(activity.ChunksTotal)), activity.CompletionPercent, activity.ChunksPerSecond)
		if activity.ETASeconds > 0 {
			progress += " • ETA " + formatDashboardDuration(activity.ETASeconds*1000)
		}
	}
	status := activity.NotReadyReason
	if status == "" {
		status = "fresh"
	}
	return fmt.Sprintf("%s • %s • %s • updated %s\r\n%s\r\n%s", activity.InstanceID, activity.Repository, activity.Stage, updated, progress, status)
}

func dashboardGPUText(snapshot sweexplore.DashboardSnapshot) string {
	live := snapshot.Activity.GPUTelemetry
	liveText := "Live GPU: no current telemetry"
	if live != nil {
		liveText = fmt.Sprintf("Live GPU: %d%% • %s / %s MiB\r\n%.1f W • %d C • sampled %s", live.UtilizationPercent, formatDashboardInt(int64(live.MemoryUsedMiB)), formatDashboardInt(int64(live.MemoryTotalMiB)), live.PowerWatts, live.TemperatureCelsius, dashboardAge(live.SampledAt))
	}
	recorded := snapshot.Preparation
	if recorded.Samples == 0 {
		return liveText + "\r\nRecorded preparation: unavailable"
	}
	return liveText + fmt.Sprintf("\r\nRecorded: peak %d%% • %s MiB • %.1f W • %d C\r\n%d/%d samples >=80%% • %s chunks • %s", recorded.PeakGPUUtilization, formatDashboardInt(int64(recorded.PeakMemoryUsedMiB)), recorded.PeakPowerWatts, recorded.PeakTemperatureCelsius, recorded.GPUActiveSamples, recorded.Samples, formatDashboardInt(int64(recorded.CommittedChunks)), formatDashboardDuration(recorded.DurationMS))
}

func dashboardResultsText(snapshot sweexplore.DashboardSnapshot) string {
	var builder strings.Builder
	for _, provider := range []sweexplore.Provider{sweexplore.ProviderCodex, sweexplore.ProviderClaude} {
		aggregate := snapshot.Providers[provider]
		fmt.Fprintf(&builder, "%s  •  %d completed pair(s)\r\n", strings.ToUpper(string(provider)), aggregate.CompletedPairs)
		if aggregate.CompletedPairs == 0 {
			builder.WriteString("  No validated pair evidence yet.\r\n\r\n")
			continue
		}
		fmt.Fprintf(&builder, "  avg elapsed: native %s  |  LCTK %s  |  LCTK calls %d", averageDashboardElapsed(aggregate.Native), averageDashboardElapsed(aggregate.LCTK), aggregate.LCTK.TotalLCTKCalls)
		if aggregate.Native.TotalProviderStats.ReportedCostUSD > 0 || aggregate.LCTK.TotalProviderStats.ReportedCostUSD > 0 {
			fmt.Fprintf(&builder, "  |  cost $%.3f -> $%.3f", aggregate.Native.TotalProviderStats.ReportedCostUSD, aggregate.LCTK.TotalProviderStats.ReportedCostUSD)
		}
		builder.WriteString("\r\n")
		builder.WriteString("  metric                      native       LCTK      delta            95% CI\r\n")
		for _, metric := range []string{"context_efficiency", "first_useful_hit", "recall_at_300", "weighted_core_coverage"} {
			interval := aggregate.PrimaryBootstrap95CI[metric]
			fmt.Fprintf(&builder, "  %-25s %9.4f  %9.4f  %+9.4f  [%+.4f, %+.4f]\r\n", metric, aggregate.Native.MeanMetrics[metric], aggregate.LCTK.MeanMetrics[metric], aggregate.MeanPairedDelta[metric], interval.Lower, interval.Upper)
		}
		builder.WriteString("\r\n")
	}
	return builder.String()
}

func averageDashboardElapsed(aggregate sweexplore.ArmAggregate) string {
	if aggregate.Runs == 0 {
		return "—"
	}
	return formatDashboardDuration(aggregate.TotalElapsedMS / int64(aggregate.Runs))
}

func formatDashboardDuration(milliseconds int64) string {
	if milliseconds <= 0 {
		return "0s"
	}
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration >= time.Hour {
		return fmt.Sprintf("%.1fh", duration.Hours())
	}
	if duration >= time.Minute {
		return fmt.Sprintf("%.1fm", duration.Minutes())
	}
	return fmt.Sprintf("%.1fs", duration.Seconds())
}

func formatDashboardInt(value int64) string {
	digits := strconv.FormatInt(value, 10)
	start := 0
	if strings.HasPrefix(digits, "-") {
		start = 1
	}
	for index := len(digits) - 3; index > start; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

func dashboardAge(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "unknown"
	}
	age := time.Since(parsed)
	if age < 0 {
		age = 0
	}
	if age < time.Minute {
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	}
	return fmt.Sprintf("%.1fh ago", age.Hours())
}

func (window *nativeDashboardWindow) handleCommand(identifier uint16) {
	switch identifier {
	case idRefresh:
		window.requestRefresh()
	case idPrepareNext:
		window.confirmAndStart(runPrepareNext)
	case idRunNext:
		window.confirmAndStart(runNextInstance)
	case idRunAll:
		window.confirmAndStart(runAllRemaining)
	case idStop:
		window.stopOwnedRunner()
	}
}

func (window *nativeDashboardWindow) requestRefresh() {
	select {
	case window.refresh <- struct{}{}:
	default:
	}
}

func (window *nativeDashboardWindow) confirmAndStart(mode dashboardRunMode) {
	window.mu.RLock()
	snapshot := window.snapshot
	window.mu.RUnlock()
	action := "prepare the next instance using local GPU inference"
	if mode == runNextInstance {
		action = "run the next instance and invoke paid clients"
	}
	if mode == runAllRemaining {
		action = "run every remaining instance and invoke paid clients"
	}
	if !confirmDashboard(window.window, "Confirm campaign action", fmt.Sprintf("This will %s.\r\n\r\nCampaign: %s\r\nNext ordinal: %d\r\n\r\nContinue?", action, snapshot.CampaignID, snapshot.FirstIncompleteOrdinal)) {
		return
	}
	process, err := startCampaignProcess(window.options, snapshot, mode)
	if err != nil {
		showDashboardError(window.window, "Campaign did not start", err.Error())
		return
	}
	window.mu.Lock()
	window.runner = process
	window.runnerStatus = "Running " + string(mode) + " • progress requires growing counters, traces, or receipts"
	window.mu.Unlock()
	window.render()
	go func() {
		err := process.wait()
		window.mu.Lock()
		if window.runner == process {
			window.runner = nil
		}
		if err != nil {
			window.runnerStatus = "Runner stopped with error: " + err.Error()
		} else {
			window.runnerStatus = "Runner completed successfully"
		}
		window.mu.Unlock()
		window.requestRefresh()
	}()
}

func (window *nativeDashboardWindow) stopOwnedRunner() {
	window.mu.RLock()
	runner := window.runner
	window.mu.RUnlock()
	if runner == nil || !confirmDashboard(window.window, "Stop campaign", "Stop only the campaign process tree started by this dashboard?") {
		return
	}
	window.mu.Lock()
	window.runnerStatus = "Stopping owned campaign process tree..."
	window.mu.Unlock()
	runner.stop()
	window.render()
}

func dashboardWindowProc(handle uintptr, message uint32, wParam, lParam uintptr) uintptr {
	value, found := dashboardWindows.Load(handle)
	if !found {
		result, _, _ := procDefWindowProcW.Call(handle, uintptr(message), wParam, lParam)
		return result
	}
	window := value.(*nativeDashboardWindow)
	switch message {
	case wmAppSnapshot:
		window.render()
		return 0
	case wmCommand:
		window.handleCommand(uint16(wParam & 0xffff))
		return 0
	case wmClose:
		window.mu.RLock()
		running := window.runner != nil
		window.mu.RUnlock()
		if running && !confirmDashboard(handle, "Close dashboard", "Closing will stop the campaign process tree started by this dashboard. Continue?") {
			return 0
		}
		if running {
			window.mu.RLock()
			runner := window.runner
			window.mu.RUnlock()
			if runner != nil {
				runner.stop()
			}
		}
		procDestroyWindow.Call(handle)
		return 0
	case wmDestroy:
		window.cancel()
		dashboardWindows.Delete(handle)
		procPostQuitMessage.Call(0)
		return 0
	default:
		result, _, _ := procDefWindowProcW.Call(handle, uintptr(message), wParam, lParam)
		return result
	}
}

func setDashboardText(handle uintptr, text string) {
	pointer := mustDashboardUTF16(text)
	procSetWindowTextW.Call(handle, uintptr(unsafe.Pointer(pointer)))
}

func setDashboardProgress(handle uintptr, current, total int) {
	if total <= 0 {
		total = 1
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	procSendMessageW.Call(handle, pbmSetRange32, 0, uintptr(total))
	procSendMessageW.Call(handle, pbmSetPos, uintptr(current), 0)
}

func setDashboardEnabled(handle uintptr, enabled bool) {
	value := uintptr(0)
	if enabled {
		value = 1
	}
	procEnableWindow.Call(handle, value)
}

func showDashboardError(owner uintptr, title, message string) {
	procMessageBoxW.Call(owner, uintptr(unsafe.Pointer(mustDashboardUTF16(message))), uintptr(unsafe.Pointer(mustDashboardUTF16(title))), 0x10)
}

func confirmDashboard(owner uintptr, title, message string) bool {
	result, _, _ := procMessageBoxW.Call(owner, uintptr(unsafe.Pointer(mustDashboardUTF16(message))), uintptr(unsafe.Pointer(mustDashboardUTF16(title))), 0x00000004|0x00000030|0x00000100)
	return result == 6
}

func mustDashboardUTF16(value string) *uint16 {
	encoded := utf16.Encode([]rune(value + "\x00"))
	return &encoded[0]
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
