//go:build windows

package uninstall

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestNormalizeWSLOutputAcceptsUTF16LEAndUTF8(t *testing.T) {
	want := "docker-desktop\r\nlctk-runtime\r\n"
	units := append([]uint16{0xfeff}, utf16.Encode([]rune(want))...)
	encoded := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(encoded[index*2:], unit)
	}
	if got := normalizeWSLOutput(encoded); got != want {
		t.Fatalf("UTF-16 output=%q want=%q", got, want)
	}
	if got := normalizeWSLOutput([]byte(want)); got != want {
		t.Fatalf("UTF-8 output=%q want=%q", got, want)
	}
}
