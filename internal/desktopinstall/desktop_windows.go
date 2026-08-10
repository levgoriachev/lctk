//go:build windows

package desktopinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath          = `Software\Microsoft\Windows\CurrentVersion\Run`
	userShellFoldersKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
)

var (
	resolveKnownPrograms = func() (string, error) {
		return windows.KnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_DEFAULT)
	}
	resolveRegistryPrograms = startMenuProgramsFromRegistry
)

var procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")

var (
	classShellLink = windows.GUID{Data1: 0x00021401, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidShellLinkW  = windows.GUID{Data1: 0x000214f9, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidPersistFile = windows.GUID{Data1: 0x0000010b, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

type comObject struct {
	VTable *[21]uintptr
}

func registerDesktop(launcher, uninstaller, version string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open sign-in startup registry: %w", err)
	}
	if err := key.SetStringValue("LCTK", `"`+launcher+`" daemon`); err != nil {
		key.Close()
		return fmt.Errorf("register sign-in daemon: %w", err)
	}
	if err := key.Close(); err != nil {
		return fmt.Errorf("close sign-in startup registry: %w", err)
	}
	programs, err := startMenuPrograms()
	if err != nil {
		return fmt.Errorf("locate Start menu: %w", err)
	}
	dir := filepath.Join(programs, "LCTK")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Start-menu group: %w", err)
	}
	shortcut := filepath.Join(dir, "LCTK.lnk")
	if err := createShortcut(uninstaller, "--admin", "Open LCTK", shortcut); err != nil {
		return fmt.Errorf("create Start-menu shortcut: %w", err)
	}
	uninstallShortcut := filepath.Join(dir, "Uninstall LCTK.lnk")
	if err := createShortcut(uninstaller, "--uninstall", "Remove LCTK and its managed runtime", uninstallShortcut); err != nil {
		return fmt.Errorf("create Start-menu uninstall shortcut: %w", err)
	}
	uninstall, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\LCTK`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open uninstall registry: %w", err)
	}
	defer uninstall.Close()
	for name, value := range map[string]string{
		"DisplayName":          "LCTK",
		"DisplayVersion":       version,
		"Publisher":            "LCTK contributors",
		"UninstallString":      `"` + uninstaller + `" --uninstall`,
		"QuietUninstallString": `"` + uninstaller + `" --uninstall`,
		"InstallLocation":      filepath.Dir(filepath.Dir(launcher)),
		"DisplayIcon":          uninstaller,
	} {
		if err := uninstall.SetStringValue(name, value); err != nil {
			return fmt.Errorf("register uninstaller: %w", err)
		}
	}
	if err := uninstall.SetDWordValue("NoModify", 1); err != nil {
		return fmt.Errorf("disable unsupported installation modification: %w", err)
	}
	if err := uninstall.SetDWordValue("NoRepair", 1); err != nil {
		return fmt.Errorf("disable unsupported installation repair: %w", err)
	}
	return nil
}

func unregisterDesktop() error {
	var cleanupErrors []error
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err == nil {
		if deleteErr := key.DeleteValue("LCTK"); deleteErr != nil && deleteErr != registry.ErrNotExist {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove sign-in daemon registration: %w", deleteErr))
		}
		_ = key.Close()
	} else if err != registry.ErrNotExist {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("open sign-in daemon registration: %w", err))
	}
	programs, err := startMenuPrograms()
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("locate Start menu: %w", err))
	} else if err := os.RemoveAll(filepath.Join(programs, "LCTK")); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove Start-menu group: %w", err))
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\LCTK`); err != nil && err != registry.ErrNotExist {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove uninstall registration: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

// startMenuPrograms resolves the current user's physical Programs directory.
// FOLDERID_StartMenuAllPrograms is a virtual aggregate and can legitimately
// return ERROR_FILE_NOT_FOUND, while FOLDERID_Programs is the writable folder
// that normally owns per-user shortcuts. Windows can transiently fail that API
// during uninstall, so the Explorer User Shell Folders value is the official
// relocated-folder fallback instead of a guessed profile-relative path.
func startMenuPrograms() (string, error) {
	programs, knownErr := resolveKnownPrograms()
	if knownErr == nil {
		return validateProgramsPath(programs)
	}
	programs, registryErr := resolveRegistryPrograms()
	if registryErr != nil {
		return "", errors.Join(knownErr, registryErr)
	}
	return validateProgramsPath(programs)
}

// startMenuProgramsFromRegistry resolves relocated per-user shell folders from
// the Windows Explorer contract used by the operating system itself.
func startMenuProgramsFromRegistry() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, userShellFoldersKey, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("open Explorer user shell folders: %w", err)
	}
	defer key.Close()
	programs, valueType, err := key.GetStringValue("Programs")
	if err != nil {
		return "", fmt.Errorf("read Explorer Programs folder: %w", err)
	}
	if valueType == registry.EXPAND_SZ {
		programs, err = registry.ExpandString(programs)
		if err != nil {
			return "", fmt.Errorf("expand Explorer Programs folder: %w", err)
		}
	}
	return programs, nil
}

// validateProgramsPath rejects an empty or relative registry value before any
// recursive removal can use it as a filesystem boundary.
func validateProgramsPath(programs string) (string, error) {
	programs = filepath.Clean(programs)
	if programs == "." || !filepath.IsAbs(programs) {
		return "", fmt.Errorf("Start-menu Programs path is not absolute: %q", programs)
	}
	return programs, nil
}

// createShortcut uses the operating system's IShellLinkW implementation
// directly. Shelling out to PowerShell would make spaces and non-ASCII user
// profile paths part of a second command-language parser.
func createShortcut(target, arguments, description, shortcut string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil {
		return fmt.Errorf("initialize Windows shortcut COM apartment: %w", err)
	}
	defer windows.CoUninitialize()

	var shellLink *comObject
	hresult, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&classShellLink)), 0, windows.CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&shellLink)),
	)
	if failedHRESULT(hresult) || shellLink == nil {
		return hresultError("create IShellLinkW", hresult)
	}
	defer releaseCOM(shellLink)

	targetText, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	workingText, err := windows.UTF16PtrFromString(filepath.Dir(target))
	if err != nil {
		return err
	}
	argumentsText, err := windows.UTF16PtrFromString(arguments)
	if err != nil {
		return err
	}
	descriptionText, err := windows.UTF16PtrFromString(description)
	if err != nil {
		return err
	}
	settings := []struct {
		name  string
		index int
		value *uint16
	}{
		{"set shortcut target", 20, targetText},
		{"set shortcut working directory", 9, workingText},
		{"set shortcut arguments", 11, argumentsText},
		{"set shortcut description", 7, descriptionText},
	}
	for _, setting := range settings {
		hresult, _, _ = syscall.SyscallN(shellLink.VTable[setting.index], uintptr(unsafe.Pointer(shellLink)), uintptr(unsafe.Pointer(setting.value)))
		if failedHRESULT(hresult) {
			return hresultError(setting.name, hresult)
		}
	}

	var persistFile *comObject
	hresult, _, _ = syscall.SyscallN(
		shellLink.VTable[0], uintptr(unsafe.Pointer(shellLink)), uintptr(unsafe.Pointer(&iidPersistFile)), uintptr(unsafe.Pointer(&persistFile)),
	)
	if failedHRESULT(hresult) || persistFile == nil {
		return hresultError("query IPersistFile", hresult)
	}
	defer releaseCOM(persistFile)
	shortcutText, err := windows.UTF16PtrFromString(shortcut)
	if err != nil {
		return err
	}
	// IPersistFile::Save is vtable slot 6; TRUE records this path as current.
	hresult, _, _ = syscall.SyscallN(persistFile.VTable[6], uintptr(unsafe.Pointer(persistFile)), uintptr(unsafe.Pointer(shortcutText)), 1)
	if failedHRESULT(hresult) {
		return hresultError("save shortcut", hresult)
	}
	return nil
}

func releaseCOM(object *comObject) {
	if object != nil {
		syscall.SyscallN(object.VTable[2], uintptr(unsafe.Pointer(object)))
	}
}

func failedHRESULT(value uintptr) bool { return int32(value) < 0 }

func hresultError(operation string, value uintptr) error {
	return fmt.Errorf("%s failed with HRESULT 0x%08x", operation, uint32(value))
}
