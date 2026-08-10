//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/lev-goryachev/lctk/internal/adminclient"
	"github.com/lev-goryachev/lctk/internal/adminsession"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/localapi"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
	"golang.org/x/sys/windows"
)

const (
	adminWindowWidth  = 1120
	adminWindowHeight = 890

	idAdminProjectPath    = 2001
	idAdminBrowse         = 2002
	idAdminProfile        = 2003
	idAdminAdd            = 2004
	idAdminProjects       = 2005
	idAdminStart          = 2006
	idAdminStop           = 2007
	idAdminRestart        = 2008
	idAdminReindex        = 2009
	idAdminCopyURL        = 2010
	idAdminMode           = 2011
	idAdminApplyMode      = 2012
	idAdminRequests       = 2013
	idAdminApprove        = 2014
	idAdminRefresh        = 2015
	idAdminUninstall      = 2016
	idAdminDeny           = 2017
	idAdminAuthorizations = 2018
	idAdminRevoke         = 2019

	wsVScroll       = 0x00200000
	esMultiline     = 0x0004
	esAutoVScroll   = 0x0040
	esReadOnly      = 0x0800
	emGetSel        = 0x00B0
	emSetSel        = 0x00B1
	emLineScroll    = 0x00B6
	emGetFirstLine  = 0x00CE
	wmCopy          = 0x0301
	lbsNotify       = 0x0001
	cbsDropDownList = 0x0003
	wmAppAdminState = 0x8002
	lbnSelChange    = 1
	lbResetContent  = 0x0184
	lbAddString     = 0x0180
	lbGetCurSel     = 0x0188
	lbSetCurSel     = 0x0186
	cbAddString     = 0x0143
	cbGetCurSel     = 0x0147
	cbSetCurSel     = 0x014E
	pbmSetPos       = wmUser + 2
	pbmSetRange32   = wmUser + 6
)

var (
	adminWindowClass = mustUTF16(adminWindowClassName)
	adminWindows     sync.Map
)

// nativeAdminWindow renders the daemon-owned administrator state with Win32
// controls. It deliberately contains no project authority or filesystem logic;
// every mutation goes through the authenticated loopback API.
type nativeAdminWindow struct {
	client  *adminclient.Client
	window  uintptr
	appIcon uintptr
	fonts   nativeFonts
	context context.Context
	cancel  context.CancelFunc

	statusLabel           uintptr
	projectPath           uintptr
	profile               uintptr
	projectList           uintptr
	projectInfo           uintptr
	exactProgress         uintptr
	exactProgressLabel    uintptr
	semanticProgress      uintptr
	semanticProgressLabel uintptr
	graphProgress         uintptr
	graphProgressLabel    uintptr
	mode                  uintptr
	requestList           uintptr
	authorizationList     uintptr
	logs                  uintptr
	controls              []uintptr
	buttons               map[uint16]uintptr
	projectItems          []adminListItem
	requestItems          []adminListItem
	authorizationItems    []adminListItem
	projectInfoText       string
	logsText              string

	mu       sync.RWMutex
	snapshot adminclient.Snapshot
	status   string
	failure  string
	busy     bool
}

// adminListItem keeps the stable server identity aligned with one rendered row.
// A polling refresh may reorder or update labels, so an array index is never a
// durable selection identity.
type adminListItem struct {
	ID   string
	Text string
}

// adminProgressState is a truthful rendering contract for one indexing layer.
// Determinate bars require a measured numerator and denominator; operations
// whose backend has no denominator use marquee mode instead of a fake percent.
type adminProgressState struct {
	Label         string
	Current       int
	Total         int
	Indeterminate bool
}

// runAdminWindow ensures the background daemon is reachable, spends its
// current exchange code directly from this process, and enters the native
// message loop. Nothing is opened in or delegated to a browser.
func runAdminWindow(parent context.Context, address string) error {
	if err := ensureAdminDaemon(parent, address); err != nil {
		return err
	}
	code, err := adminsession.ReadCode("")
	if err != nil {
		return err
	}
	connectCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	client, err := adminclient.Connect(connectCtx, address, code)
	cancel()
	if err != nil {
		return err
	}
	window, err := newNativeAdminWindow(parent, client)
	if err != nil {
		return err
	}
	defer window.cancel()
	window.refresh("Loading LCTK status...")
	go window.pollOAuth()
	go window.pollProjects()
	procShowWindow.Call(window.window, swShow)
	procUpdateWindow.Call(window.window)

	var current message
	for {
		result, _, callErr := procGetMessageW.Call(uintptr(unsafe.Pointer(&current)), 0, 0, 0)
		if int32(result) == -1 {
			return fmt.Errorf("read admin window message: %w", callErr)
		}
		if result == 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&current)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&current)))
	}
}

// pollOAuth keeps incoming approval requests and completed client exchanges
// visible without making the owner press Refresh. It touches only the two OAuth
// lists, so a slow runtime probe cannot delay an approval prompt.
func (window *nativeAdminWindow) pollOAuth() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-window.context.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(window.context, 1500*time.Millisecond)
			authorizations, requests, err := window.client.LoadOAuth(ctx)
			cancel()
			if err != nil {
				continue
			}
			window.mu.Lock()
			window.snapshot.Authorizations, window.snapshot.Requests = authorizations, requests
			window.mu.Unlock()
			window.postState()
		}
	}
}

// pollProjects refreshes live indexing state independently of owner actions.
// A failed or slow probe is skipped rather than replacing the last trustworthy
// snapshot, and the next tick retries without overlapping the previous probe.
func (window *nativeAdminWindow) pollProjects() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-window.context.Done():
			return
		case <-ticker.C:
			window.mu.RLock()
			busy := window.busy
			window.mu.RUnlock()
			if busy {
				continue
			}
			ctx, cancel := context.WithTimeout(window.context, 5*time.Second)
			projects, err := window.client.LoadProjects(ctx)
			cancel()
			if err != nil {
				continue
			}
			window.mu.Lock()
			window.snapshot.Projects = projects
			window.mu.Unlock()
			window.postState()
		}
	}
}

func ensureAdminDaemon(parent context.Context, address string) error {
	if adminDaemonReady(parent, address) {
		return nil
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	launcher := filepath.Join(home, "bin", "lctk.exe")
	command := exec.Command(launcher, "daemon", "--listen", address)
	windowsprocess.HideConsole(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start LCTK daemon: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("detach LCTK daemon: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if adminDaemonReady(parent, address) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("LCTK daemon did not become ready within 15 seconds")
}

func adminDaemonReady(parent context.Context, address string) bool {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/health", nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func newNativeAdminWindow(parent context.Context, client *adminclient.Client) (*nativeAdminWindow, error) {
	instance, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idArrow)
	largeIcon, smallIcon, err := loadApplicationIcons(instance)
	if err != nil {
		return nil, err
	}
	class := windowClassEx{
		Size: uint32(unsafe.Sizeof(windowClassEx{})), WindowProc: windows.NewCallback(adminWindowProc),
		Instance: instance, Icon: largeIcon, Cursor: cursor, Background: colorButtonFace + 1, ClassName: adminWindowClass, SmallIcon: smallIcon,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class)))
	controls := initCommonControls{Size: uint32(unsafe.Sizeof(initCommonControls{})), ICC: 0x20}
	procInitCommon.Call(uintptr(unsafe.Pointer(&controls)))
	screenWidth, _, _ := procGetSystemMetrics.Call(0)
	screenHeight, _, _ := procGetSystemMetrics.Call(1)
	x := (int32(screenWidth) - adminWindowWidth) / 2
	y := (int32(screenHeight) - adminWindowHeight) / 2
	handle, _, callErr := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(adminWindowClass)), uintptr(unsafe.Pointer(mustUTF16("LCTK"))),
		wsOverlappedWindow, uintptr(x), uintptr(y), adminWindowWidth, adminWindowHeight,
		0, 0, instance, 0,
	)
	if handle == 0 {
		return nil, fmt.Errorf("create native admin window: %w", callErr)
	}
	fonts, err := newNativeFonts()
	if err != nil {
		procDestroyWindow.Call(handle)
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	window := &nativeAdminWindow{client: client, window: handle, appIcon: largeIcon, fonts: fonts, context: ctx, cancel: cancel, status: "Connecting...", buttons: map[uint16]uintptr{}}
	adminWindows.Store(handle, window)
	if err := window.createControls(instance); err != nil {
		adminWindows.Delete(handle)
		procDestroyWindow.Call(handle)
		window.fonts.close()
		cancel()
		return nil, err
	}
	window.render()
	return window, nil
}

func (window *nativeAdminWindow) createControls(instance uintptr) error {
	create := func(className, text string, style uint32, x, y, width, height int32, id uintptr) (uintptr, error) {
		handle, _, callErr := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(mustUTF16(className))), uintptr(unsafe.Pointer(mustUTF16(text))), uintptr(style),
			uintptr(x), uintptr(y), uintptr(width), uintptr(height), window.window, id, instance, 0,
		)
		if handle == 0 {
			return 0, fmt.Errorf("create %s admin control: %w", className, callErr)
		}
		setNativeFont(handle, window.fonts.body)
		return handle, nil
	}
	label := func(text string, x, y, width, height int32) error {
		handle, err := create("STATIC", text, wsChild|wsVisible|ssLeft, x, y, width, height, 0)
		if err == nil {
			setNativeFont(handle, window.fonts.section)
		}
		return err
	}
	brandIcon, err := create("STATIC", "", wsChild|wsVisible|ssIcon, 24, 10, 48, 48, 0)
	if err != nil {
		return err
	}
	procSendMessageW.Call(brandIcon, stmSetIcon, window.appIcon, 0)
	window.statusLabel, err = create("STATIC", "", wsChild|wsVisible|ssLeft, 100, 18, 980, 46, 0)
	if err != nil {
		return err
	}
	setNativeFont(window.statusLabel, window.fonts.title)
	if err := label("Add project", 24, 67, 160, 22); err != nil {
		return err
	}
	window.projectPath, err = create("EDIT", "", wsChild|wsVisible|wsTabStop|wsBorder|esAutoHScroll, 24, 92, 670, 28, idAdminProjectPath)
	if err != nil {
		return err
	}
	styleNativeEdit(window.projectPath)
	browse, err := create("BUTTON", "Browse...", wsChild|wsVisible|wsTabStop|bsPushButton, 704, 91, 88, 30, idAdminBrowse)
	if err != nil {
		return err
	}
	window.controls = append(window.controls, browse)
	window.profile, err = create("COMBOBOX", "", wsChild|wsVisible|wsTabStop|wsVScroll|cbsDropDownList, 802, 91, 100, 180, idAdminProfile)
	if err != nil {
		return err
	}
	styleNativeCombo(window.profile)
	addComboItems(window.profile, []string{"minimal", "full"}, 0)
	add, err := create("BUTTON", "Add", wsChild|wsVisible|wsTabStop|bsPushButton, 912, 91, 170, 30, idAdminAdd)
	if err != nil {
		return err
	}
	window.controls = append(window.controls, add)

	if err := label("Projects", 24, 136, 160, 22); err != nil {
		return err
	}
	window.projectList, err = create("LISTBOX", "", wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|lbsNotify, 24, 160, 430, 284, idAdminProjects)
	if err != nil {
		return err
	}
	styleNativeList(window.projectList)
	window.projectInfo, err = create("EDIT", "Select a project.", wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll|esReadOnly, 470, 160, 612, 88, 0)
	if err != nil {
		return err
	}
	styleNativeEdit(window.projectInfo)
	progressSpecs := []struct {
		name  string
		y     int32
		label *uintptr
		bar   *uintptr
	}{
		{name: "Exact", y: 252, label: &window.exactProgressLabel, bar: &window.exactProgress},
		{name: "Semantic", y: 284, label: &window.semanticProgressLabel, bar: &window.semanticProgress},
		{name: "Graph", y: 316, label: &window.graphProgressLabel, bar: &window.graphProgress},
	}
	for _, spec := range progressSpecs {
		*spec.label, err = create("STATIC", spec.name+": unavailable", wsChild|wsVisible|ssLeft, 470, spec.y, 612, 16, 0)
		if err != nil {
			return err
		}
		*spec.bar, err = create("msctls_progress32", "", wsChild|wsVisible|pbsMarquee, 470, spec.y+17, 612, 12, 0)
		if err != nil {
			return err
		}
	}
	buttonSpecs := []struct {
		text        string
		id          uintptr
		x, y, width int32
	}{
		{"Start", idAdminStart, 470, 352, 82}, {"Stop", idAdminStop, 560, 352, 82},
		{"Restart", idAdminRestart, 650, 352, 82}, {"Reindex", idAdminReindex, 740, 352, 82},
		{"Copy MCP setup", idAdminCopyURL, 830, 352, 252},
	}
	for _, spec := range buttonSpecs {
		button, createErr := create("BUTTON", spec.text, wsChild|wsVisible|wsTabStop|bsPushButton, spec.x, spec.y, spec.width, 31, spec.id)
		if createErr != nil {
			return createErr
		}
		window.controls = append(window.controls, button)
		window.buttons[uint16(spec.id)] = button
	}
	if err := label("Resource mode", 470, 403, 110, 22); err != nil {
		return err
	}
	window.mode, err = create("COMBOBOX", "", wsChild|wsVisible|wsTabStop|wsVScroll|cbsDropDownList, 585, 398, 120, 180, idAdminMode)
	if err != nil {
		return err
	}
	styleNativeCombo(window.mode)
	addComboItems(window.mode, []string{"quiet", "normal", "fast"}, 1)
	applyMode, err := create("BUTTON", "Apply mode", wsChild|wsVisible|wsTabStop|bsPushButton, 716, 397, 130, 31, idAdminApplyMode)
	if err != nil {
		return err
	}
	window.controls = append(window.controls, applyMode)
	window.buttons[idAdminApplyMode] = applyMode

	if err := label("Pending connection requests", 24, 450, 250, 22); err != nil {
		return err
	}
	window.requestList, err = create("LISTBOX", "", wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|lbsNotify, 24, 474, 822, 82, idAdminRequests)
	if err != nil {
		return err
	}
	styleNativeList(window.requestList)
	approve, err := create("BUTTON", "Approve selected", wsChild|wsVisible|wsTabStop|bsPushButton, 860, 474, 222, 32, idAdminApprove)
	if err != nil {
		return err
	}
	deny, err := create("BUTTON", "Deny selected", wsChild|wsVisible|wsTabStop|bsPushButton, 860, 514, 222, 32, idAdminDeny)
	if err != nil {
		return err
	}
	window.controls = append(window.controls, approve, deny)
	window.buttons[idAdminApprove], window.buttons[idAdminDeny] = approve, deny

	if err := label("Authorized clients", 24, 568, 180, 22); err != nil {
		return err
	}
	window.authorizationList, err = create("LISTBOX", "", wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|lbsNotify, 24, 592, 822, 76, idAdminAuthorizations)
	if err != nil {
		return err
	}
	styleNativeList(window.authorizationList)
	revoke, err := create("BUTTON", "Revoke selected", wsChild|wsVisible|wsTabStop|bsPushButton, 860, 592, 222, 32, idAdminRevoke)
	if err != nil {
		return err
	}
	window.controls = append(window.controls, revoke)
	window.buttons[idAdminRevoke] = revoke

	if err := label("Daemon log", 24, 680, 160, 22); err != nil {
		return err
	}
	window.logs, err = create("EDIT", "", wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll|esReadOnly, 24, 704, 822, 105, 0)
	if err != nil {
		return err
	}
	setNativeFont(window.logs, window.fonts.mono)
	styleNativeEdit(window.logs)
	refresh, err := create("BUTTON", "Refresh", wsChild|wsVisible|wsTabStop|bsPushButton, 860, 704, 222, 34, idAdminRefresh)
	if err != nil {
		return err
	}
	uninstallButton, err := create("BUTTON", "Uninstall LCTK", wsChild|wsVisible|wsTabStop|bsPushButton, 860, 775, 222, 34, idAdminUninstall)
	if err != nil {
		return err
	}
	window.controls = append(window.controls, refresh, uninstallButton)
	return nil
}

func adminWindowProc(windowHandle uintptr, message uint32, wParam, lParam uintptr) uintptr {
	value, exists := adminWindows.Load(windowHandle)
	if !exists {
		result, _, _ := procDefWindowProcW.Call(windowHandle, uintptr(message), wParam, lParam)
		return result
	}
	window := value.(*nativeAdminWindow)
	switch message {
	case wmCtlColorStatic:
		// Read-only EDIT controls use WM_CTLCOLORSTATIC too. Leaving those
		// surfaces to DefWindowProc keeps the log and project details visually
		// distinct from labels on the system dialog background.
		if lParam != window.projectInfo && lParam != window.logs {
			return styleStaticBackground(wParam)
		}
	case wmCommand:
		id := uint16(wParam & 0xffff)
		notification := uint16((wParam >> 16) & 0xffff)
		if id == idAdminProjects && notification == lbnSelChange {
			window.renderSelection()
			return 0
		}
		if (id == idAdminRequests || id == idAdminAuthorizations) && notification == lbnSelChange {
			window.renderSelection()
			return 0
		}
		window.handleCommand(id)
		return 0
	case wmAppAdminState:
		window.render()
		return 0
	case wmClose:
		window.cancel()
		procDestroyWindow.Call(window.window)
		return 0
	case wmDestroy:
		window.fonts.close()
		adminWindows.Delete(windowHandle)
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProcW.Call(windowHandle, uintptr(message), wParam, lParam)
	return result
}

func (window *nativeAdminWindow) handleCommand(id uint16) {
	switch id {
	case idAdminBrowse:
		if selected := browseForFolder(window.window, "Choose a project directory"); selected != "" {
			setWindowText(window.projectPath, selected)
		}
	case idAdminAdd:
		path := strings.TrimSpace(windowText(window.projectPath))
		profile := selectedComboText(window.profile, []string{"minimal", "full"})
		if path == "" {
			window.fail("Choose a project directory first.")
			return
		}
		window.act("Registering project...", func(ctx context.Context) error { return window.client.AddProject(ctx, path, profile) })
	case idAdminStart:
		window.projectAction("start", "Starting project...")
	case idAdminStop:
		window.projectAction("stop", "Stopping project...")
	case idAdminRestart:
		window.projectAction("restart", "Restarting project...")
	case idAdminReindex:
		window.projectAction("reindex", "Reindexing project...")
	case idAdminCopyURL:
		if _, ok := window.selectedProject(); !ok {
			window.fail("Select a project first.")
			return
		}
		procSendMessageW.Call(window.projectInfo, emSetSel, 0, ^uintptr(0))
		procSendMessageW.Call(window.projectInfo, wmCopy, 0, 0)
		window.mu.Lock()
		window.status, window.failure = "MCP connection instructions copied.", ""
		window.mu.Unlock()
		window.postState()
	case idAdminApplyMode:
		project, ok := window.selectedProject()
		if !ok {
			window.fail("Select a project first.")
			return
		}
		mode := selectedComboText(window.mode, []string{"quiet", "normal", "fast"})
		window.act("Changing resource mode...", func(ctx context.Context) error { return window.client.SetProjectMode(ctx, project.ID, mode) })
	case idAdminRevoke:
		authorization, ok := window.selectedAuthorization()
		if !ok {
			window.fail("Select an authorized client first.")
			return
		}
		window.act("Revoking client authorization...", func(ctx context.Context) error { return window.client.RevokeAuthorization(ctx, authorization.ID) })
	case idAdminApprove, idAdminDeny:
		request, ok := window.selectedAuthorizationRequest()
		if !ok {
			window.fail("Select a pending connection request first.")
			return
		}
		decision := "approve"
		status := "Approving connection..."
		if id == idAdminApprove {
			message := fmt.Sprintf("Authorize this exact MCP connection?\r\n\r\nClient: %s\r\nProject: %s\r\nCallback: %s\r\nScopes: %s\r\nExpires: %s", request.Client, request.Project, request.RedirectURI, strings.Join(request.Scopes, " "), request.ExpiresAt)
			answer, _ := windows.MessageBox(windows.HWND(window.window), mustUTF16(message), mustUTF16("Approve MCP connection"), windows.MB_YESNO|windows.MB_ICONWARNING)
			if answer != messageBoxYes {
				return
			}
		} else {
			decision, status = "deny", "Denying connection..."
		}
		window.act(status, func(ctx context.Context) error { return window.client.DecideAuthorization(ctx, request.ID, decision) })
	case idAdminRefresh:
		window.refresh("Refreshing LCTK status...")
	case idAdminUninstall:
		answer, _ := windows.MessageBox(windows.HWND(window.window), mustUTF16("Open the complete LCTK uninstall dialog?"), mustUTF16("Uninstall LCTK"), windows.MB_YESNO|windows.MB_ICONWARNING)
		if answer != messageBoxYes {
			return
		}
		window.launchUninstaller()
	}
}

func (window *nativeAdminWindow) projectAction(action, status string) {
	project, ok := window.selectedProject()
	if !ok {
		window.fail("Select a project first.")
		return
	}
	window.act(status, func(ctx context.Context) error { return window.client.ProjectAction(ctx, project.ID, action) })
}

func (window *nativeAdminWindow) selectedProject() (adminclient.Project, bool) {
	id, ok := selectedListID(window.projectList, window.projectItems)
	if !ok {
		return adminclient.Project{}, false
	}
	window.mu.RLock()
	defer window.mu.RUnlock()
	for _, project := range window.snapshot.Projects {
		if project.ID == id {
			return project, true
		}
	}
	return adminclient.Project{}, false
}

func (window *nativeAdminWindow) selectedAuthorization() (adminclient.Authorization, bool) {
	id, ok := selectedListID(window.authorizationList, window.authorizationItems)
	if !ok {
		return adminclient.Authorization{}, false
	}
	window.mu.RLock()
	defer window.mu.RUnlock()
	for _, authorization := range window.snapshot.Authorizations {
		if authorization.ID == id {
			return authorization, true
		}
	}
	return adminclient.Authorization{}, false
}

func (window *nativeAdminWindow) selectedAuthorizationRequest() (adminclient.AuthorizationRequest, bool) {
	id, ok := selectedListID(window.requestList, window.requestItems)
	if !ok {
		return adminclient.AuthorizationRequest{}, false
	}
	window.mu.RLock()
	defer window.mu.RUnlock()
	for _, request := range window.snapshot.Requests {
		if request.ID == id {
			return request, true
		}
	}
	return adminclient.AuthorizationRequest{}, false
}

func (window *nativeAdminWindow) act(status string, operation func(context.Context) error) {
	window.mu.Lock()
	if window.busy {
		window.mu.Unlock()
		return
	}
	window.busy, window.status, window.failure = true, status, ""
	window.mu.Unlock()
	window.postState()
	go func() {
		ctx, cancel := context.WithTimeout(window.context, 4*time.Minute)
		err := operation(ctx)
		cancel()
		if err == nil {
			ctx, cancel = context.WithTimeout(window.context, 3*time.Minute)
			snapshot, loadErr := window.client.Load(ctx)
			cancel()
			if loadErr == nil {
				window.mu.Lock()
				window.snapshot = snapshot
				window.mu.Unlock()
			}
			err = loadErr
		}
		window.mu.Lock()
		window.busy = false
		if err != nil {
			window.failure, window.status = err.Error(), "The action failed."
		} else {
			window.failure, window.status = "", "Ready."
		}
		window.mu.Unlock()
		window.postState()
	}()
}

func (window *nativeAdminWindow) refresh(status string) {
	window.mu.Lock()
	if window.busy {
		window.mu.Unlock()
		return
	}
	window.busy, window.status, window.failure = true, status, ""
	window.mu.Unlock()
	window.postState()
	go func() {
		ctx, cancel := context.WithTimeout(window.context, 3*time.Minute)
		snapshot, err := window.client.Load(ctx)
		cancel()
		window.mu.Lock()
		window.busy = false
		if err != nil {
			window.failure, window.status = err.Error(), "Could not load LCTK status."
		} else {
			window.snapshot, window.failure, window.status = snapshot, "", "Ready."
		}
		window.mu.Unlock()
		window.postState()
	}()
}

func (window *nativeAdminWindow) launchUninstaller() {
	window.mu.Lock()
	if window.busy {
		window.mu.Unlock()
		return
	}
	window.busy, window.status, window.failure = true, "Opening the uninstaller...", ""
	window.mu.Unlock()
	window.postState()
	go func() {
		ctx, cancel := context.WithTimeout(window.context, 10*time.Second)
		err := window.client.LaunchUninstaller(ctx)
		cancel()
		if err != nil {
			window.mu.Lock()
			window.busy, window.failure, window.status = false, err.Error(), "Could not open the uninstaller."
			window.mu.Unlock()
			window.postState()
			return
		}
		procPostMessageW.Call(window.window, wmClose, 0, 0)
	}()
}

func (window *nativeAdminWindow) fail(message string) {
	window.mu.Lock()
	window.failure, window.status = message, "Action required."
	window.mu.Unlock()
	window.postState()
}

func (window *nativeAdminWindow) postState() {
	procPostMessageW.Call(window.window, wmAppAdminState, 0, 0)
}

func (window *nativeAdminWindow) render() {
	window.mu.RLock()
	snapshot, status, failure, busy := window.snapshot, window.status, window.failure, window.busy
	window.mu.RUnlock()
	runtimeText := "runtime unavailable"
	if snapshot.Overview.Runtime.Available {
		runtimeText = strings.TrimSpace(snapshot.Overview.Runtime.Provider + " " + snapshot.Overview.Runtime.Version + " " + snapshot.Overview.Runtime.OSType)
	} else if snapshot.Overview.Runtime.Detail != "" {
		runtimeText += ": " + snapshot.Overview.Runtime.Detail
	}
	header := status
	inferenceText := "inference unavailable"
	if snapshot.Overview.Inference.Ready {
		inferenceText = "inference " + snapshot.Overview.Inference.Backend
		if snapshot.Overview.Inference.GPU != nil {
			inferenceText += " " + snapshot.Overview.Inference.GPU.Name
		}
	} else if snapshot.Overview.Inference.Detail != "" {
		inferenceText += ": " + snapshot.Overview.Inference.Detail
	}
	if snapshot.Overview.Version != "" {
		header = fmt.Sprintf("LCTK %s | %s | %s | %s", snapshot.Overview.Version, runtimeText, inferenceText, status)
	}
	if failure != "" {
		header += "  Error: " + failure
	}
	setWindowText(window.statusLabel, header)
	projects := make([]adminListItem, 0, len(snapshot.Projects))
	for _, project := range snapshot.Projects {
		text := fmt.Sprintf("%s  |  %s  |  %s  |  %s", project.Name, project.State, project.Mode, project.Path)
		projects = append(projects, adminListItem{ID: project.ID, Text: text})
	}
	updateListBox(window.projectList, &window.projectItems, projects)
	requests := make([]adminListItem, 0, len(snapshot.Requests))
	for _, request := range snapshot.Requests {
		text := fmt.Sprintf("%s  |  project %s  |  callback %s  |  expires %s", request.Client, request.Project, request.RedirectURI, request.ExpiresAt)
		requests = append(requests, adminListItem{ID: request.ID, Text: text})
	}
	updateListBox(window.requestList, &window.requestItems, requests)
	authorizations := make([]adminListItem, 0, len(snapshot.Authorizations))
	for _, authorization := range snapshot.Authorizations {
		state := "active"
		if authorization.Revoked {
			state = "revoked"
		}
		text := fmt.Sprintf("%s  |  project %s  |  %s  |  %s", authorization.Client, authorization.Project, authorization.IssuedAt, state)
		authorizations = append(authorizations, adminListItem{ID: authorization.ID, Text: text})
	}
	updateListBox(window.authorizationList, &window.authorizationItems, authorizations)
	updateReadOnlyEdit(window.logs, &window.logsText, renderAdminLogs(snapshot))
	setEnabled(window.projectPath, !busy)
	setEnabled(window.profile, !busy)
	for _, control := range window.controls {
		setEnabled(control, !busy)
	}
	window.renderSelection()
}

func (window *nativeAdminWindow) renderSelection() {
	window.mu.RLock()
	busy := window.busy
	window.mu.RUnlock()
	project, ok := window.selectedProject()
	if !ok {
		updateReadOnlyEdit(window.projectInfo, &window.projectInfoText, "No project selected.")
		window.renderIndexProgress(nil)
	} else {
		index := "exact unavailable"
		if project.Index != nil {
			index = fmt.Sprintf("exact %d files, generation %d", project.Index.FileCount, project.Index.Generation)
			if project.Index.Indexing {
				index += " (indexing)"
			}
			if semantic := project.Index.Semantic; semantic != nil {
				index += "; semantic " + semanticDiagnostic(semantic)
			}
			if graph := project.Index.Graph; graph != nil {
				if graph.Ready {
					index += fmt.Sprintf("; graph %s generation %d, %d nodes", graph.Freshness, graph.Generation, graph.NodeCount)
				} else {
					index += "; graph not ready"
					if graph.Reason != "" {
						index += ": " + graph.Reason
					}
				}
			}
		}
		changes := "not watched"
		if project.Changes != nil {
			changes = fmt.Sprintf("%d pending, %d directories", project.Changes.Pending, project.Changes.Directories)
			if !project.Changes.Complete {
				changes += ", incomplete: " + project.Changes.GapReason
			}
			if project.Changes.LastError != "" {
				changes += ", last error: " + project.Changes.LastError
			}
		}
		endpoint := fmt.Sprintf("http://%s/projects/%s/mcp", localapi.DefaultAddress, project.ID)
		detail := fmt.Sprintf("%s\r\nMCP URL: %s\r\nCodex: remove any older LCTK entry, then Settings > MCP servers > Add server > Streamable HTTP > paste URL > Save/Restart > Authenticate.\r\nState: %s; health: %s; index: %s; changes: %s; disk: %s", project.Name, endpoint, project.State, project.Health, index, changes, project.Disk.Human)
		if project.Detail != "" {
			detail += "\r\n" + project.Detail
		}
		updateReadOnlyEdit(window.projectInfo, &window.projectInfoText, detail)
		window.renderIndexProgress(project.Index)
		modes := []string{"quiet", "normal", "fast"}
		for index, mode := range modes {
			if mode == project.Mode {
				procSendMessageW.Call(window.mode, cbSetCurSel, uintptr(index), 0)
				break
			}
		}
	}
	running := ok && project.State == "running"
	setEnabled(window.buttons[idAdminStart], !busy && ok && !running)
	for _, id := range []uint16{idAdminStop, idAdminRestart, idAdminReindex} {
		setEnabled(window.buttons[id], !busy && running)
	}
	setEnabled(window.buttons[idAdminCopyURL], !busy && ok)
	setEnabled(window.buttons[idAdminApplyMode], !busy && ok)
	setEnabled(window.mode, !busy && ok)
	_, authorizationSelected := window.selectedAuthorization()
	setEnabled(window.buttons[idAdminRevoke], !busy && authorizationSelected)
	_, requestSelected := window.selectedAuthorizationRequest()
	setEnabled(window.buttons[idAdminApprove], !busy && requestSelected)
	setEnabled(window.buttons[idAdminDeny], !busy && requestSelected)
}

// renderIndexProgress keeps exact, semantic, and graph status independently
// visible. This prevents a ready exact index from visually masking an active or
// failed derived-index build.
func (window *nativeAdminWindow) renderIndexProgress(index *adminclient.Index) {
	exact, semantic, graph := indexProgressStates(index)
	setAdminProgress(window.exactProgressLabel, window.exactProgress, exact)
	setAdminProgress(window.semanticProgressLabel, window.semanticProgress, semantic)
	setAdminProgress(window.graphProgressLabel, window.graphProgress, graph)
}

func setAdminProgress(labelHandle, barHandle uintptr, state adminProgressState) {
	setWindowText(labelHandle, state.Label)
	if state.Indeterminate {
		procSendMessageW.Call(barHandle, pbmSetMarquee, 1, 30)
		return
	}
	procSendMessageW.Call(barHandle, pbmSetMarquee, 0, 0)
	total := state.Total
	if total <= 0 {
		total = 1
	}
	current := state.Current
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	procSendMessageW.Call(barHandle, pbmSetRange32, 0, uintptr(total))
	procSendMessageW.Call(barHandle, pbmSetPos, uintptr(current), 0)
}

func indexProgressStates(index *adminclient.Index) (adminProgressState, adminProgressState, adminProgressState) {
	unavailable := func(name string) adminProgressState { return adminProgressState{Label: name + ": unavailable"} }
	if index == nil {
		return unavailable("Exact"), unavailable("Semantic"), unavailable("Graph")
	}

	exact := adminProgressState{Label: fmt.Sprintf("Exact: ready — %d files, generation %d", index.FileCount, index.Generation), Current: 1, Total: 1}
	if index.Indexing {
		exact = adminProgressState{Label: "Exact: indexing — total is not reported", Indeterminate: true}
	} else if !index.Ready {
		exact = adminProgressState{Label: "Exact: not ready"}
		if index.Reason != "" {
			exact.Label += " — " + index.Reason
		}
	}

	semantic := adminProgressState{Label: "Semantic: unavailable"}
	if value := index.Semantic; value != nil {
		completed := value.ChunksEmbedded + value.ChunksReused
		switch {
		case value.Stalled:
			semantic = adminProgressState{Label: fmt.Sprintf("Semantic: STALLED for %ds — %d/%d chunks", value.StallSeconds, completed, value.ChunksTotal), Current: completed, Total: value.ChunksTotal}
		case value.Indexing && value.ChunksTotal > 0:
			semantic = adminProgressState{Label: fmt.Sprintf("Semantic: indexing — %d/%d chunks", completed, value.ChunksTotal), Current: completed, Total: value.ChunksTotal}
		case value.Indexing:
			semantic = adminProgressState{Label: "Semantic: preparing chunks", Indeterminate: true}
		case value.Ready:
			semantic = adminProgressState{Label: fmt.Sprintf("Semantic: %s — %d chunks, generation %d", value.Freshness, value.ChunkCount, value.Generation), Current: 1, Total: 1}
		case value.LastError != "":
			semantic = adminProgressState{Label: "Semantic: failed — " + value.LastError}
		default:
			semantic = adminProgressState{Label: "Semantic: not ready"}
			if value.Reason != "" {
				semantic.Label += " — " + value.Reason
			}
		}
	}

	graph := adminProgressState{Label: "Graph: unavailable"}
	if value := index.Graph; value != nil {
		switch {
		case index.Indexing || (index.Semantic != nil && index.Semantic.Indexing):
			graph = adminProgressState{Label: "Graph: building — progress total is not reported", Indeterminate: true}
		case value.Ready:
			graph = adminProgressState{Label: fmt.Sprintf("Graph: %s — %d nodes, generation %d", value.Freshness, value.NodeCount, value.Generation), Current: 1, Total: 1}
		default:
			graph = adminProgressState{Label: "Graph: not ready"}
			if value.Reason != "" {
				graph.Label += " — " + value.Reason
			}
		}
	}
	return exact, semantic, graph
}

func semanticDiagnostic(semantic *adminclient.SemanticIndex) string {
	if semantic.Stalled {
		return fmt.Sprintf("STALLED for %ds at %d/%d chunks", semantic.StallSeconds, semantic.ChunksEmbedded+semantic.ChunksReused, semantic.ChunksTotal)
	}
	if semantic.Indexing {
		progress := "indexing"
		if semantic.ChunksTotal > 0 {
			progress = fmt.Sprintf("indexing %d/%d chunks", semantic.ChunksEmbedded+semantic.ChunksReused, semantic.ChunksTotal)
		}
		if semantic.LastError != "" {
			progress += ", previous error: " + semantic.LastError
		}
		return progress
	}
	if semantic.Ready {
		return fmt.Sprintf("%s generation %d, %d chunks", semantic.Freshness, semantic.Generation, semantic.ChunkCount)
	}
	if semantic.LastError != "" {
		return "failed: " + semantic.LastError
	}
	if semantic.Reason != "" {
		return "not ready: " + semantic.Reason
	}
	return "not ready"
}

func selectedListID(handle uintptr, items []adminListItem) (string, bool) {
	index, _, _ := procSendMessageW.Call(handle, lbGetCurSel, 0, 0)
	if int32(index) < 0 || int(index) >= len(items) {
		return "", false
	}
	return items[index].ID, true
}

// updateListBox avoids destructive redraws when a poll returned identical
// content and restores changed lists by stable server ID rather than by index.
// This is what keeps an OAuth request selected while the two-second poll runs.
func updateListBox(handle uintptr, rendered *[]adminListItem, next []adminListItem) {
	selectedID, _ := selectedListID(handle, *rendered)
	if equalAdminListItems(*rendered, next) {
		return
	}
	procSendMessageW.Call(handle, lbResetContent, 0, 0)
	for _, item := range next {
		procSendMessageW.Call(handle, lbAddString, 0, uintptr(unsafe.Pointer(mustUTF16(item.Text))))
	}
	*rendered = append((*rendered)[:0], next...)
	for index, item := range next {
		if item.ID == selectedID {
			procSendMessageW.Call(handle, lbSetCurSel, uintptr(index), 0)
			break
		}
	}
}

func equalAdminListItems(first, second []adminListItem) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

// updateReadOnlyEdit preserves both the selected text and the scroll position.
// Polling identical content does no Win32 mutation at all; changed log content
// restores the same UTF-16 selection when that text still exists.
func updateReadOnlyEdit(handle uintptr, rendered *string, next string) {
	if *rendered == next {
		return
	}
	var start, end uint32
	procSendMessageW.Call(handle, emGetSel, uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&end)))
	firstLine, _, _ := procSendMessageW.Call(handle, emGetFirstLine, 0, 0)
	nextStart, nextEnd := preservedUTF16Selection(*rendered, next, int(start), int(end))
	setWindowText(handle, next)
	procSendMessageW.Call(handle, emSetSel, uintptr(nextStart), uintptr(nextEnd))
	currentLine, _, _ := procSendMessageW.Call(handle, emGetFirstLine, 0, 0)
	delta := int64(firstLine) - int64(currentLine)
	if delta != 0 {
		procSendMessageW.Call(handle, emLineScroll, 0, uintptr(delta))
	}
	*rendered = next
}

func preservedUTF16Selection(previous, next string, start, end int) (int, int) {
	previousUnits := utf16.Encode([]rune(previous))
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if start > len(previousUnits) {
		start = len(previousUnits)
	}
	if end > len(previousUnits) {
		end = len(previousUnits)
	}
	if start != end {
		selected := string(utf16.Decode(previousUnits[start:end]))
		nextUnits := utf16.Encode([]rune(next))
		if end <= len(nextUnits) && string(utf16.Decode(nextUnits[start:end])) == selected {
			return start, end
		}
		if byteIndex := strings.Index(next, selected); byteIndex >= 0 {
			nextStart := len(utf16.Encode([]rune(next[:byteIndex])))
			return nextStart, nextStart + len(utf16.Encode([]rune(selected)))
		}
		end = start
	}
	nextLength := len(utf16.Encode([]rune(next)))
	if start > nextLength {
		start = nextLength
	}
	if end > nextLength {
		end = nextLength
	}
	return start, end
}

func addComboItems(handle uintptr, values []string, selected int) {
	for _, value := range values {
		procSendMessageW.Call(handle, cbAddString, 0, uintptr(unsafe.Pointer(mustUTF16(value))))
	}
	procSendMessageW.Call(handle, cbSetCurSel, uintptr(selected), 0)
}

func selectedComboText(handle uintptr, values []string) string {
	index, _, _ := procSendMessageW.Call(handle, cbGetCurSel, 0, 0)
	if int32(index) < 0 || int(index) >= len(values) {
		return ""
	}
	return values[index]
}

func renderAdminLogs(snapshot adminclient.Snapshot) string {
	lines := make([]string, 0, len(snapshot.Logs))
	start := 0
	if len(snapshot.Logs) > 200 {
		start = len(snapshot.Logs) - 200
	}
	for _, record := range snapshot.Logs[start:] {
		keys := make([]string, 0, len(record.Fields))
		for key := range record.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		line := fmt.Sprintf("%s  %s  %s", record.At.Format(time.RFC3339), record.Level, record.Message)
		for _, key := range keys {
			line += "  " + key + "=" + record.Fields[key]
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "Nothing logged yet."
	}
	return strings.Join(lines, "\r\n")
}
