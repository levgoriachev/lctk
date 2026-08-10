//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/lev-goryachev/lctk/internal/diskspace"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/setupflow"
	"golang.org/x/sys/windows"
)

const (
	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsTabStop          = 0x00010000
	wsBorder           = 0x00800000
	esAutoHScroll      = 0x0080
	bsPushButton       = 0x00000000
	ssLeft             = 0x00000000
	ssIcon             = 0x00000003
	pbsMarquee         = 0x00000008

	wmDestroy  = 0x0002
	wmClose    = 0x0010
	wmCommand  = 0x0111
	wmSetFont  = 0x0030
	wmUser     = 0x0400
	wmAppState = 0x8001
	stmSetIcon = 0x0170

	pbmSetMarquee = wmUser + 10
	swShow        = 5
	idArrow       = 32512

	idBrowseInstall = 1001
	idBrowseRuntime = 1002
	idInstall       = 1003

	bifReturnOnlyFSDirs = 0x0001
	bifNewDialogStyle   = 0x0040
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	comctl32             = windows.NewLazySystemDLL("comctl32.dll")
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procSetWindowTextW   = user32.NewProc("SetWindowTextW")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	procEnableWindow     = user32.NewProc("EnableWindow")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procInitCommon       = comctl32.NewProc("InitCommonControlsEx")
	procBrowseForFolderW = shell32.NewProc("SHBrowseForFolderW")
	procGetPathFromID    = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")

	setupWindowClass = mustUTF16("LCTKNativeSetupWindow")
	setupWindows     sync.Map
)

type point struct {
	X int32
	Y int32
}

type message struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   point
	Private uint32
}

type windowClassEx struct {
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

type initCommonControls struct {
	Size uint32
	ICC  uint32
}

type browseInfo struct {
	Owner       uintptr
	Root        uintptr
	DisplayName *uint16
	Title       *uint16
	Flags       uint32
	Callback    uintptr
	Parameter   uintptr
	Image       int32
}

type nativeSetupWindow struct {
	request setupRequest
	window  uintptr
	appIcon uintptr
	fonts   nativeFonts

	installEdit       uintptr
	runtimeEdit       uintptr
	browseInstall     uintptr
	browseRuntime     uintptr
	installButton     uintptr
	distributionCombo uintptr
	statusLabel       uintptr
	errorLabel        uintptr
	progress          uintptr

	context context.Context
	cancel  context.CancelFunc

	mu         sync.RWMutex
	status     string
	failure    string
	installing bool
	complete   bool
}

func runSetupWindow(request setupRequest) error {
	window, err := newNativeSetupWindow(request)
	if err != nil {
		return err
	}
	defer window.cancel()
	procShowWindow.Call(window.window, swShow)
	procUpdateWindow.Call(window.window)

	var current message
	for {
		result, _, callErr := procGetMessageW.Call(uintptr(unsafe.Pointer(&current)), 0, 0, 0)
		if int32(result) == -1 {
			return fmt.Errorf("read setup window message: %w", callErr)
		}
		if result == 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&current)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&current)))
	}
}

func newNativeSetupWindow(request setupRequest) (*nativeSetupWindow, error) {
	instance, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idArrow)
	largeIcon, smallIcon, err := loadApplicationIcons(instance)
	if err != nil {
		return nil, err
	}
	class := windowClassEx{
		Size: uint32(unsafe.Sizeof(windowClassEx{})), WindowProc: windows.NewCallback(setupWindowProc),
		Instance: instance, Icon: largeIcon, Cursor: cursor, Background: colorButtonFace + 1, ClassName: setupWindowClass, SmallIcon: smallIcon,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		// A second setup instance can reuse the process-global class. Any other
		// registration failure is surfaced by the following CreateWindowEx call.
		_ = callErr
	}
	controls := initCommonControls{Size: uint32(unsafe.Sizeof(initCommonControls{})), ICC: 0x20}
	procInitCommon.Call(uintptr(unsafe.Pointer(&controls)))
	const windowWidth, windowHeight = 780, 620
	screenWidth, _, _ := procGetSystemMetrics.Call(0)
	screenHeight, _, _ := procGetSystemMetrics.Call(1)
	x := (int32(screenWidth) - windowWidth) / 2
	y := (int32(screenHeight) - windowHeight) / 2
	handle, _, callErr := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(setupWindowClass)), uintptr(unsafe.Pointer(mustUTF16(setupTitle(request.Action)))),
		wsOverlappedWindow, uintptr(x), uintptr(y), windowWidth, windowHeight,
		0, 0, instance, 0,
	)
	if handle == 0 {
		return nil, fmt.Errorf("create native setup window: %w", callErr)
	}
	fonts, err := newNativeFonts()
	if err != nil {
		procDestroyWindow.Call(handle)
		return nil, err
	}
	ctx, cancel := context.WithCancel(request.Context)
	window := &nativeSetupWindow{request: request, window: handle, appIcon: largeIcon, fonts: fonts, context: ctx, cancel: cancel,
		status: setupInitialStatus(request.Action)}
	setupWindows.Store(handle, window)
	if err := window.createControls(instance); err != nil {
		setupWindows.Delete(handle)
		procDestroyWindow.Call(handle)
		window.fonts.close()
		cancel()
		return nil, err
	}
	window.render()
	return window, nil
}

func (window *nativeSetupWindow) createControls(instance uintptr) error {
	create := func(className, text string, style uint32, x, y, width, height int32, id uintptr) (uintptr, error) {
		handle, _, callErr := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(mustUTF16(className))), uintptr(unsafe.Pointer(mustUTF16(text))), uintptr(style),
			uintptr(x), uintptr(y), uintptr(width), uintptr(height), window.window, id, instance, 0,
		)
		if handle == 0 {
			return 0, fmt.Errorf("create %s setup control: %w", className, callErr)
		}
		setNativeFont(handle, window.fonts.body)
		return handle, nil
	}
	brandIcon, err := create("STATIC", "", wsChild|wsVisible|ssIcon, 688, 18, 48, 48, 0)
	if err != nil {
		return err
	}
	procSendMessageW.Call(brandIcon, stmSetIcon, window.appIcon, 0)
	title, err := create("STATIC", setupTitle(window.request.Action), wsChild|wsVisible|ssLeft, 28, 22, 640, 34, 0)
	if err != nil {
		return err
	}
	setNativeFont(title, window.fonts.title)
	labels := []struct {
		text                string
		x, y, width, height int32
		section             bool
	}{
		{"One private runtime. No Docker Desktop or build tools.", 28, 60, 640, 24, false},
		{fmt.Sprintf("Version %s  |  Windows build %d, %s  |  WSL2 %s", window.request.Manifest.Version, window.request.Host.Build, window.request.Host.Architecture, readyText(window.request.Host.WSLReady)), 28, 93, 700, 24, false},
		{"Installation directory", 28, 132, 700, 22, true},
		{"Runtime data directory (WSL disk, images, project indexes and memory)", 28, 207, 700, 22, true},
		{"Local inference distribution", 28, 282, 700, 22, true},
	}
	for _, label := range labels {
		handle, err := create("STATIC", label.text, wsChild|wsVisible|ssLeft, label.x, label.y, label.width, label.height, 0)
		if err != nil {
			return err
		}
		if label.section {
			setNativeFont(handle, window.fonts.section)
		}
	}
	window.installEdit, err = create("EDIT", window.request.Locations.InstallDir, wsChild|wsVisible|wsTabStop|wsBorder|esAutoHScroll, 28, 158, 600, 30, 0)
	if err != nil {
		return err
	}
	styleNativeEdit(window.installEdit)
	window.browseInstall, err = create("BUTTON", "Browse...", wsChild|wsVisible|wsTabStop|bsPushButton, 642, 158, 94, 30, idBrowseInstall)
	if err != nil {
		return err
	}
	if window.request.InstallLocked {
		setEnabled(window.installEdit, false)
		setEnabled(window.browseInstall, false)
	}
	window.runtimeEdit, err = create("EDIT", window.request.Locations.RuntimeDataDir, wsChild|wsVisible|wsTabStop|wsBorder|esAutoHScroll, 28, 233, 600, 30, 0)
	if err != nil {
		return err
	}
	styleNativeEdit(window.runtimeEdit)
	window.browseRuntime, err = create("BUTTON", "Browse...", wsChild|wsVisible|wsTabStop|bsPushButton, 642, 233, 94, 30, idBrowseRuntime)
	if err != nil {
		return err
	}
	window.distributionCombo, err = create("COMBOBOX", "", wsChild|wsVisible|wsTabStop|wsVScroll|cbsDropDownList, 28, 308, 420, 120, 0)
	if err != nil {
		return err
	}
	styleNativeCombo(window.distributionCombo)
	for _, option := range []string{"CPU - supported on every compatible Windows host", "NVIDIA GPU - requires a verified Pascal-or-newer adapter"} {
		procSendMessageW.Call(window.distributionCombo, cbAddString, 0, uintptr(unsafe.Pointer(mustUTF16(option))))
	}
	selectedDistribution := uintptr(0)
	if window.request.Distribution == inference.DistributionNVIDIAGPU {
		selectedDistribution = 1
	}
	procSendMessageW.Call(window.distributionCombo, cbSetCurSel, selectedDistribution, 0)
	lockText := "Both locations can be changed before LCTK and its managed WSL machine are created."
	if window.request.InstallLocked && window.request.RuntimeLocked {
		lockText = "The existing installation and lctk-runtime machine keep their current locations; setup will continue them in place."
	} else if window.request.InstallLocked {
		lockText = "The existing LCTK installation keeps its current location; runtime data can still be selected before machine creation."
	} else if window.request.RuntimeLocked {
		lockText = "The existing lctk-runtime machine keeps its current location; setup will continue it without migration."
	}
	if window.request.RuntimeLocked {
		setEnabled(window.runtimeEdit, false)
		setEnabled(window.browseRuntime, false)
	}
	if _, err := create("STATIC", lockText, wsChild|wsVisible|ssLeft, 28, 350, 708, 40, 0); err != nil {
		return err
	}
	window.statusLabel, err = create("STATIC", "", wsChild|wsVisible|ssLeft, 28, 400, 708, 42, 0)
	if err != nil {
		return err
	}
	window.errorLabel, err = create("STATIC", "", wsChild|wsVisible|ssLeft, 28, 445, 708, 45, 0)
	if err != nil {
		return err
	}
	window.progress, err = create("msctls_progress32", "", wsChild|wsVisible|pbsMarquee, 28, 516, 490, 20, 0)
	if err != nil {
		return err
	}
	window.installButton, err = create("BUTTON", setupButtonText(window.request.Action), wsChild|wsVisible|wsTabStop|bsPushButton, 548, 507, 188, 38, idInstall)
	return err
}

func setupWindowProc(windowHandle uintptr, message uint32, wParam, lParam uintptr) uintptr {
	value, exists := setupWindows.Load(windowHandle)
	if !exists {
		result, _, _ := procDefWindowProcW.Call(windowHandle, uintptr(message), wParam, lParam)
		return result
	}
	window := value.(*nativeSetupWindow)
	switch message {
	case wmCtlColorStatic:
		return styleStaticBackground(wParam)
	case wmCommand:
		switch uint16(wParam & 0xffff) {
		case idBrowseInstall:
			if selected := browseForFolder(window.window, "Choose the LCTK installation directory"); selected != "" {
				setWindowText(window.installEdit, selected)
			}
		case idBrowseRuntime:
			if selected := browseForFolder(window.window, "Choose the LCTK runtime-data directory"); selected != "" {
				setWindowText(window.runtimeEdit, selected)
			}
		case idInstall:
			window.startOrClose()
		}
		return 0
	case wmAppState:
		window.render()
		return 0
	case wmClose:
		window.mu.RLock()
		installing := window.installing
		window.mu.RUnlock()
		if installing {
			text := mustUTF16("Installation is still running. Cancel it and close setup?")
			title := mustUTF16("LCTK Setup")
			answer, _ := windows.MessageBox(windows.HWND(window.window), text, title, windows.MB_YESNO|windows.MB_ICONWARNING)
			if answer != messageBoxYes {
				return 0
			}
		}
		window.cancel()
		procDestroyWindow.Call(window.window)
		return 0
	case wmDestroy:
		window.fonts.close()
		setupWindows.Delete(windowHandle)
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProcW.Call(windowHandle, uintptr(message), wParam, lParam)
	return result
}

func (window *nativeSetupWindow) startOrClose() {
	window.mu.RLock()
	complete, installing := window.complete, window.installing
	window.mu.RUnlock()
	if complete {
		procDestroyWindow.Call(window.window)
		return
	}
	if installing {
		return
	}
	locations, err := lctkhome.NormalizeLocations(windowText(window.installEdit), windowText(window.runtimeEdit))
	if err != nil {
		window.finish(err, false)
		return
	}
	distribution, err := selectedDistribution(window.distributionCombo)
	if err != nil {
		window.finish(err, false)
		return
	}
	window.request.Distribution = distribution
	if window.request.RuntimeLocked && !strings.EqualFold(locations.RuntimeDataDir, window.request.Locations.RuntimeDataDir) {
		window.finish(errors.New("the runtime-data directory cannot change while lctk-runtime exists"), false)
		return
	}
	if window.request.InstallLocked && !strings.EqualFold(locations.InstallDir, window.request.Locations.InstallDir) {
		window.finish(errors.New("the installation directory cannot change while LCTK is activated"), false)
		return
	}
	window.mu.Lock()
	window.installing = true
	window.failure = ""
	window.status = "Recalculating the verified installation plan..."
	window.mu.Unlock()
	window.postState()
	go window.install(locations)
}

func (window *nativeSetupWindow) install(locations lctkhome.Locations) {
	_, plan, err := inspectSelection(window.context, window.request, locations)
	if err != nil {
		window.finish(err, false)
		return
	}
	if !plan.Ready {
		window.finish(errors.New("the selected locations do not have enough free space or the host is not ready"), false)
		return
	}
	confirmation := setupConfirmation(plan, locations)
	answer, _ := windows.MessageBox(windows.HWND(window.window), mustUTF16(confirmation), mustUTF16("Review LCTK "+string(plan.Action)), windows.MB_YESNO|windows.MB_ICONINFORMATION)
	if answer != messageBoxYes {
		window.mu.Lock()
		window.installing = false
		window.status = "No changes were made. You can review the plan again or close setup."
		window.mu.Unlock()
		window.postState()
		return
	}
	err = applySelection(window.context, window.request, locations, func(_, detail string) {
		window.mu.Lock()
		window.status = detail
		window.mu.Unlock()
		window.postState()
	})
	if errors.Is(err, setupflow.ErrRebootRequired) {
		window.mu.Lock()
		window.installing = false
		window.complete = true
		window.status = err.Error()
		window.mu.Unlock()
		window.postState()
		return
	}
	window.finish(err, err == nil)
}

func (window *nativeSetupWindow) finish(err error, complete bool) {
	window.mu.Lock()
	window.installing = false
	window.complete = complete
	if err != nil {
		window.failure = err.Error()
		window.status = "Setup stopped without reporting success."
	} else if complete {
		window.failure = ""
		window.status = setupCompleteStatus(window.request.Action)
	}
	window.mu.Unlock()
	window.postState()
}

func (window *nativeSetupWindow) postState() {
	procPostMessageW.Call(window.window, wmAppState, 0, 0)
}

func (window *nativeSetupWindow) render() {
	window.mu.RLock()
	status, failure := window.status, window.failure
	installing, complete := window.installing, window.complete
	window.mu.RUnlock()
	setWindowText(window.statusLabel, status)
	setWindowText(window.errorLabel, failure)
	setEnabled(window.installEdit, !window.request.InstallLocked && !installing && !complete)
	setEnabled(window.browseInstall, !window.request.InstallLocked && !installing && !complete)
	setEnabled(window.runtimeEdit, !window.request.RuntimeLocked && !installing && !complete)
	setEnabled(window.browseRuntime, !window.request.RuntimeLocked && !installing && !complete)
	setEnabled(window.distributionCombo, !installing && !complete)
	setEnabled(window.installButton, !installing)
	buttonText := setupButtonText(window.request.Action)
	if complete {
		buttonText = "Close"
	}
	setWindowText(window.installButton, buttonText)
	if installing {
		procSendMessageW.Call(window.progress, pbmSetMarquee, 1, 30)
	} else {
		procSendMessageW.Call(window.progress, pbmSetMarquee, 0, 0)
	}
}

// setupTitle keeps the native surface explicit about whether it will create,
// update, or repair the recorded installation.
func setupTitle(action setupflow.Action) string {
	switch action {
	case setupflow.ActionUpgrade:
		return "Update LCTK"
	case setupflow.ActionRepair:
		return "Repair LCTK"
	default:
		return "Install LCTK"
	}
}

// setupButtonText mirrors the accepted transaction instead of presenting every
// repeated setup run as a fresh installation.
func setupButtonText(action setupflow.Action) string {
	switch action {
	case setupflow.ActionUpgrade:
		return "Review and update"
	case setupflow.ActionRepair:
		return "Review and repair"
	default:
		return "Review and install"
	}
}

// setupInitialStatus explains why existing paths are locked before the user
// opens the review dialog.
func setupInitialStatus(action setupflow.Action) string {
	switch action {
	case setupflow.ActionUpgrade:
		return "Review the in-place update. Existing locations and project data will be preserved."
	case setupflow.ActionRepair:
		return "Review the same-version repair. Existing locations and project data will be preserved."
	default:
		return "Choose where LCTK and its container data will be stored."
	}
}

// setupConfirmation is the final plan boundary before any setup mutation.
func setupConfirmation(plan setupflow.Plan, locations lctkhome.Locations) string {
	versionLine := fmt.Sprintf("Install LCTK %s?", plan.Version)
	if plan.Action == setupflow.ActionUpgrade {
		versionLine = fmt.Sprintf("Update LCTK %s to %s in place?", plan.CurrentVersion, plan.Version)
	} else if plan.Action == setupflow.ActionRepair {
		versionLine = fmt.Sprintf("Repair LCTK %s in place?", plan.Version)
	}
	inferenceLine := "CPU"
	if plan.InferenceDistribution == inference.DistributionNVIDIAGPU {
		inferenceLine = "NVIDIA GPU"
		if plan.GPU != nil {
			inferenceLine = fmt.Sprintf("NVIDIA GPU - %s, driver %s, %d MiB VRAM, compute %s",
				plan.GPU.Name, plan.GPU.DriverVersion, plan.GPU.VRAMMiB, plan.GPU.ComputeCapability)
		}
	}
	return fmt.Sprintf(
		"%s\n\nInstallation directory:\n%s\n\nRuntime data directory:\n%s\n\nInference: %s\nDownload: %s\nRuntime-data required: %s\nRuntime-data free space: %s\nRuntime: Podman %s in the managed WSL machine lctk-runtime\n\nProjects, indexes, memory, settings, and OAuth approvals are preserved.",
		versionLine, locations.InstallDir, locations.RuntimeDataDir, inferenceLine, diskspace.Human(plan.DownloadBytes), diskspace.Human(plan.RuntimeDataRequiredBytes), diskspace.Human(int64(plan.RuntimeDataAvailableBytes)), plan.Runtime.Version,
	)
}

func selectedDistribution(combo uintptr) (inference.Distribution, error) {
	index, _, _ := procSendMessageW.Call(combo, cbGetCurSel, 0, 0)
	switch int32(index) {
	case 0:
		return inference.DistributionCPU, nil
	case 1:
		return inference.DistributionNVIDIAGPU, nil
	default:
		return "", errors.New("select CPU or NVIDIA GPU inference before continuing")
	}
}

// setupCompleteStatus confirms the action that succeeded before Admin opens.
func setupCompleteStatus(action setupflow.Action) string {
	switch action {
	case setupflow.ActionUpgrade:
		return "LCTK was updated successfully. The application interface is opening."
	case setupflow.ActionRepair:
		return "LCTK was repaired successfully. The application interface is opening."
	default:
		return "LCTK is installed. The application interface is opening."
	}
}

func browseForFolder(owner uintptr, title string) string {
	var displayName [windows.MAX_PATH]uint16
	info := browseInfo{Owner: owner, DisplayName: &displayName[0], Title: mustUTF16(title), Flags: bifReturnOnlyFSDirs | bifNewDialogStyle}
	item, _, _ := procBrowseForFolderW.Call(uintptr(unsafe.Pointer(&info)))
	if item == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(item)
	var path [windows.MAX_PATH]uint16
	ok, _, _ := procGetPathFromID.Call(item, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return ""
	}
	return windows.UTF16ToString(path[:])
}

func windowText(handle uintptr) string {
	length, _, _ := procGetWindowTextLen.Call(handle)
	buffer := make([]uint16, length+1)
	procGetWindowTextW.Call(handle, uintptr(unsafe.Pointer(&buffer[0])), length+1)
	return windows.UTF16ToString(buffer)
}

func setWindowText(handle uintptr, value string) {
	if handle == 0 {
		return
	}
	procSetWindowTextW.Call(handle, uintptr(unsafe.Pointer(mustUTF16(value))))
}

func setEnabled(handle uintptr, enabled bool) {
	value := uintptr(0)
	if enabled {
		value = 1
	}
	procEnableWindow.Call(handle, value)
}

func readyText(ready bool) string {
	if ready {
		return "ready"
	}
	return "will be enabled"
}

func mustUTF16(value string) *uint16 {
	result, err := windows.UTF16PtrFromString(value)
	if err != nil {
		panic(err)
	}
	return result
}

func init() {
	// A goroutine executing setup may call MessageBox while the main OS thread
	// owns the window message loop. Keeping that loop on one thread preserves
	// Win32 window affinity without introducing a GUI framework.
	runtime.LockOSThread()
}
