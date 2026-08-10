//go:build windows

package main

import "testing"

// TestNativeFontsCanBeCreatedAndReleased verifies the exact installed-font
// boundary used by both native windows. A missing baseline font or an invalid
// CreateFontW call must fail in the focused package test before setup ships.
func TestNativeFontsCanBeCreatedAndReleased(t *testing.T) {
	fonts, err := newNativeFonts()
	if err != nil {
		t.Fatal(err)
	}
	defer fonts.close()

	for name, handle := range map[string]uintptr{
		"body": fonts.body, "section": fonts.section,
		"title": fonts.title, "mono": fonts.mono,
	} {
		if handle == 0 {
			t.Fatalf("native %s font has a zero handle", name)
		}
	}
}
