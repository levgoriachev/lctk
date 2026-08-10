//go:build windows

package main

import (
	"fmt"
	"unsafe"
)

const (
	// colorButtonFace is the system-managed dialog surface color. Using the
	// system brush keeps LCTK aligned with the active Windows accessibility and
	// personalization settings without introducing an application paint layer.
	colorButtonFace = 15

	// transparentBackground lets static labels inherit the dialog surface. The
	// text remains rendered by the native STATIC control rather than by LCTK.
	transparentBackground = 1

	// Font weights and ClearType quality define a small native typography scale
	// while preserving Windows' own text rasterization.
	fontWeightNormal   = 400
	fontWeightSemibold = 600
	clearTypeQuality   = 5
	defaultCharset     = 1

	// Native edit and list messages add breathing room to the existing controls
	// without replacing their painting, focus, selection, or accessibility.
	emSetMargins      = 0x00D3
	ecLeftMargin      = 0x0001
	ecRightMargin     = 0x0002
	lbSetItemHeight   = 0x01A0
	cbSetItemHeight   = 0x0153
	comboSelectionRow = ^uintptr(0)

	wmCtlColorStatic = 0x0138
)

var (
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procGetSysColorBrush = user32.NewProc("GetSysColorBrush")
)

// nativeFonts owns the four system-rendered fonts used by both Windows
// surfaces. Keeping the scale centralized is the Win32 equivalent of a small
// typography token set, and prevents individual controls from inventing sizes.
type nativeFonts struct {
	body    uintptr
	section uintptr
	title   uintptr
	mono    uintptr
}

// newNativeFonts creates fonts supported by the Windows 10 product baseline.
// Negative heights describe character height, which produces predictable
// native metrics for controls whose dimensions are still expressed in dialog
// pixels by the existing UI contract.
func newNativeFonts() (nativeFonts, error) {
	fonts := nativeFonts{}
	created := []*uintptr{&fonts.body, &fonts.section, &fonts.title, &fonts.mono}
	specs := []struct {
		face   string
		height int32
		weight int32
	}{
		{face: "Segoe UI", height: -15, weight: fontWeightNormal},
		{face: "Segoe UI", height: -15, weight: fontWeightSemibold},
		{face: "Segoe UI", height: -22, weight: fontWeightSemibold},
		{face: "Consolas", height: -14, weight: fontWeightNormal},
	}
	for index, spec := range specs {
		font, err := createNativeFont(spec.face, spec.height, spec.weight)
		if err != nil {
			fonts.close()
			return nativeFonts{}, err
		}
		*created[index] = font
	}
	return fonts, nil
}

// createNativeFont asks GDI to render one installed Windows font. Both Segoe UI
// and Consolas are part of the supported Windows baseline, so setup remains a
// single executable and carries no font files or framework runtime.
func createNativeFont(face string, height, weight int32) (uintptr, error) {
	handle, _, callErr := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0,
		defaultCharset, 0, 0, clearTypeQuality, 0,
		uintptr(unsafe.Pointer(mustUTF16(face))),
	)
	if handle == 0 {
		return 0, fmt.Errorf("create native %s font: %w", face, callErr)
	}
	return handle, nil
}

// close releases the process-owned GDI font handles after Windows has
// destroyed every child control that references them.
func (fonts *nativeFonts) close() {
	for _, handle := range []uintptr{fonts.body, fonts.section, fonts.title, fonts.mono} {
		if handle != 0 {
			procDeleteObject.Call(handle)
		}
	}
	*fonts = nativeFonts{}
}

// setNativeFont applies one font while leaving text layout and rendering in the
// standard Windows control implementation.
func setNativeFont(control, font uintptr) {
	procSendMessageW.Call(control, wmSetFont, font, 1)
}

// styleNativeEdit adds horizontal text insets to an existing EDIT control. It
// changes only the native control's metrics; selection, IME, and clipboard
// behavior remain owned by Windows.
func styleNativeEdit(control uintptr) {
	const inset = 7
	margins := uintptr(inset | inset<<16)
	procSendMessageW.Call(control, emSetMargins, ecLeftMargin|ecRightMargin, margins)
}

// styleNativeList increases the fixed row height of an existing LISTBOX so its
// native selection rectangle and text are less cramped.
func styleNativeList(control uintptr) {
	procSendMessageW.Call(control, lbSetItemHeight, 0, 24)
}

// styleNativeCombo aligns the closed COMBOBOX selection with the text fields
// while retaining the standard Windows drop-down and keyboard behavior.
func styleNativeCombo(control uintptr) {
	procSendMessageW.Call(control, cbSetItemHeight, comboSelectionRow, 23)
}

// styleStaticBackground makes standard STATIC controls use the parent dialog's
// system surface. Windows still draws the glyphs and icon; LCTK only selects
// the system-managed background brush.
func styleStaticBackground(deviceContext uintptr) uintptr {
	procSetBkMode.Call(deviceContext, transparentBackground)
	brush, _, _ := procGetSysColorBrush.Call(colorButtonFace)
	return brush
}
