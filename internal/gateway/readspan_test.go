package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// spanFixture builds a project over a real host directory, because read_span is
// served from the saved working tree rather than the index: it must work on a
// project with no code-intelligence service at all.
func spanFixture(t *testing.T, files map[string]string) (string, *mcp.ClientSession) {
	t.Helper()
	root := t.TempDir()
	for relative, content := range files {
		full := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newConfiguredFixture(t, fixtureConfig{
		requireRunning: true,
		roots:          map[string]string{"alpha-aaaaaaaa": root},
	}, "alpha-aaaaaaaa")
	return root, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
}

func numberedLines(from, to int, line func(int) string) string {
	var builder strings.Builder
	for number := from; number <= to; number++ {
		fmt.Fprintf(&builder, "%6d\t%s\n", number, line(number))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func generatedFile(lines int) string {
	var builder strings.Builder
	for number := 1; number <= lines; number++ {
		fmt.Fprintf(&builder, "line %d\n", number)
	}
	return builder.String()
}

func TestReadSpanReturnsNumberedLines(t *testing.T) {
	_, session := spanFixture(t, map[string]string{
		"internal/a.go": "one\ntwo\nthree\nfour\n",
	})

	text, result := callText(t, session, "read_span", map[string]any{
		"path": "internal/a.go", "start_line": 2, "end_line": 3,
	})
	if result.IsError {
		t.Fatalf("read_span refused: %s", resultText(result))
	}
	want := "     2\ttwo\n     3\tthree"
	if text != want {
		t.Errorf("span = %q, want %q", text, want)
	}
}

func TestReadSpanDefaultsToTwoHundredLinesFromTheStart(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"big.txt": generatedFile(210)})

	text, result := callText(t, session, "read_span", map[string]any{"path": "big.txt", "start_line": 5})
	if result.IsError {
		t.Fatalf("read_span refused: %s", resultText(result))
	}
	want := numberedLines(5, 204, func(n int) string { return fmt.Sprintf("line %d", n) })
	if text != want {
		t.Errorf("default span did not cover lines 5-204:\nfirst line = %q\nlast line = %q",
			strings.SplitN(text, "\n", 2)[0], text[strings.LastIndex(text, "\n")+1:])
	}
}

func TestReadSpanSaysWhereTheFileEnds(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"a.txt": "one\ntwo\nthree\n"})

	text, result := callText(t, session, "read_span", map[string]any{
		"path": "a.txt", "start_line": 1, "end_line": 999,
	})
	if result.IsError {
		t.Fatalf("read_span refused: %s", resultText(result))
	}
	want := "     1\tone\n     2\ttwo\n     3\tthree\n[file ends at line 3]"
	if text != want {
		t.Errorf("span = %q, want %q", text, want)
	}
}

func TestReadSpanClampsOversizedRanges(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"big.txt": generatedFile(2100)})

	text, result := callText(t, session, "read_span", map[string]any{
		"path": "big.txt", "start_line": 1, "end_line": 2100,
	})
	if result.IsError {
		t.Fatalf("read_span refused: %s", resultText(result))
	}
	if !strings.HasSuffix(text, "[span clamped to 2000 lines — continue from line 2001]") {
		t.Errorf("no clamp footer:\n%q", text[strings.LastIndex(text, "\n")+1:])
	}
	if strings.Contains(text, "line 2001") && !strings.Contains(text, "continue from line 2001") {
		t.Error("the span ran past the clamp")
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 2001 { // 2000 numbered lines plus the footer.
		t.Errorf("span holds %d lines, want 2000 plus the footer", len(lines)-1)
	}
}

func TestReadSpanAnswersWhenTheStartIsPastTheEnd(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"a.txt": "one\ntwo\nthree\n"})

	text, result := callText(t, session, "read_span", map[string]any{"path": "a.txt", "start_line": 10})
	if result.IsError {
		t.Fatalf("read_span refused: %s", resultText(result))
	}
	if text != "no lines: a.txt has 3 lines, requested start 10" {
		t.Errorf("answer = %q", text)
	}
}

func TestReadSpanRefusesABackwardRange(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"a.txt": "one\ntwo\n"})

	_, result := callText(t, session, "read_span", map[string]any{
		"path": "a.txt", "start_line": 5, "end_line": 2,
	})
	if !result.IsError {
		t.Fatal("a backward range was accepted")
	}
	if text := resultText(result); !strings.Contains(text, "INVALID_PATH") {
		t.Errorf("refusal = %q, want INVALID_PATH", text)
	}
}

// TestReadSpanConfinesReadsToTheProjectRoot is the boundary the tool must never
// cross: the route's scope is this project, and no path argument may read
// through it.
func TestReadSpanConfinesReadsToTheProjectRoot(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("credentials\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, session := spanFixture(t, map[string]string{
		"a.txt":     "one\n",
		"sub/b.txt": "two\n",
	})

	cases := []struct {
		name string
		path string
	}{
		{"an absolute path", secret},
		{"a parent escape", "../outside.txt"},
		{"an escape through a subdirectory", "sub/../../outside.txt"},
		{"an empty path", "   "},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, result := callText(t, session, "read_span", map[string]any{"path": testCase.path})
			if !result.IsError {
				t.Fatalf("%q was read", testCase.path)
			}
			text := resultText(result)
			if !strings.Contains(text, "INVALID_PATH") {
				t.Errorf("refusal = %q, want INVALID_PATH", text)
			}
			if strings.Contains(text, "credentials") {
				t.Error("the refusal leaked file content")
			}
		})
	}
}

func TestReadSpanRefusesASymlinkThatLeavesTheProject(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("credentials\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, session := spanFixture(t, map[string]string{"a.txt": "one\n"})
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	_, result := callText(t, session, "read_span", map[string]any{"path": "link.txt"})
	if !result.IsError {
		t.Fatal("a symlink out of the project was followed")
	}
	text := resultText(result)
	if !strings.Contains(text, "INVALID_PATH") || !strings.Contains(text, "outside the project root") {
		t.Errorf("refusal = %q", text)
	}
}

func TestReadSpanReportsAMissingFileAsSuch(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"a.txt": "one\n"})

	_, result := callText(t, session, "read_span", map[string]any{"path": "gone.txt"})
	if !result.IsError {
		t.Fatal("a missing file was read")
	}
	if text := resultText(result); !strings.Contains(text, "FILE_NOT_FOUND") {
		t.Errorf("refusal = %q, want FILE_NOT_FOUND", text)
	}
}

func TestReadSpanRefusesADirectory(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"sub/b.txt": "two\n"})

	_, result := callText(t, session, "read_span", map[string]any{"path": "sub"})
	if !result.IsError {
		t.Fatal("a directory was read as a file")
	}
	if text := resultText(result); !strings.Contains(text, "INVALID_PATH") {
		t.Errorf("refusal = %q, want INVALID_PATH", text)
	}
}

func TestReadSpanRefusesAnOversizedFile(t *testing.T) {
	_, session := spanFixture(t, map[string]string{
		"huge.txt": strings.Repeat("x", maxReadBytes+1),
	})

	_, result := callText(t, session, "read_span", map[string]any{"path": "huge.txt"})
	if !result.IsError {
		t.Fatal("an oversized file was read")
	}
	text := resultText(result)
	if !strings.Contains(text, "FILE_TOO_LARGE") || !strings.Contains(text, "file_outline") {
		t.Errorf("refusal = %q, want FILE_TOO_LARGE with an alternative", text)
	}
}

func TestReadSpanRefusesABinaryFile(t *testing.T) {
	_, session := spanFixture(t, map[string]string{"blob.bin": "abc\x00def"})

	_, result := callText(t, session, "read_span", map[string]any{"path": "blob.bin"})
	if !result.IsError {
		t.Fatal("a binary file was read as lines")
	}
	text := resultText(result)
	if !strings.Contains(text, "INVALID_PATH") || !strings.Contains(text, "binary") {
		t.Errorf("refusal = %q, want the binary explanation", text)
	}
}
