package brandassets

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestEmbeddedBrowserAssetsRetainTheirCanonicalFormats prevents a missing or
// accidentally replaced asset from silently producing an unbranded OAuth UI.
func TestEmbeddedBrowserAssetsRetainTheirCanonicalFormats(t *testing.T) {
	if len(FaviconICO) < 6 || binary.LittleEndian.Uint16(FaviconICO[2:4]) != 1 || binary.LittleEndian.Uint16(FaviconICO[4:6]) != 3 {
		t.Fatal("embedded favicon is not the accepted three-frame ICO")
	}
	if !bytes.Contains(LockupOnLightSVG, []byte(`<svg`)) || !strings.Contains(string(LockupOnLightSVG), "#31363D") || !strings.Contains(string(LockupOnLightSVG), "#0B8197") {
		t.Fatal("embedded lockup is not the canonical light-background SVG")
	}
}
