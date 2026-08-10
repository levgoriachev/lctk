package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/codeintel"
)

// exploreProject registers one project with a file and installs a running
// scripted stack plus a fake code-intelligence service.
func exploreProject(t *testing.T, service http.Handler) string {
	t.Helper()
	isolateHome(t)
	dir := makeProjectDir(t, t.TempDir(), "alpha")
	content := "package alpha\n\n// Alpha answers.\nfunc Alpha() int {\n\treturn 42\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "alpha.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := project(t, "add", dir); err != nil {
		t.Fatal(err)
	}
	useRuntime(t,
		scriptedReply{match: "info --format json", stdout: `{"host":{"os":"linux"}}`},
		scriptedReply{match: "inspect", stdout: "running|healthy|10.89.0.2\n"},
	)
	if service != nil {
		server := httptest.NewServer(service)
		t.Cleanup(server.Close)
		previous := newSearchClient
		newSearchClient = func(string) *codeintel.Client {
			return codeintel.New(strings.TrimPrefix(server.URL, "http://"))
		}
		t.Cleanup(func() { newSearchClient = previous })
	}
	return dir
}

func exploreCommand(t *testing.T, run func([]string, io.Writer) error, args ...string) string {
	t.Helper()
	var stdout bytes.Buffer
	if err := run(args, &stdout); err != nil {
		t.Fatalf("command failed: %v", err)
	}
	return stdout.String()
}

func TestSearchPrintsGrepLinesWithHostContext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		var request codeintel.Request
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Pattern != "Alpha" || request.Mode != "regex" {
			t.Errorf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"matches": []codeintel.Match{{Path: "alpha.go", Line: 4, Preview: "func Alpha() int {"}},
			"total":   1,
		})
	})
	exploreProject(t, mux)

	var stdout bytes.Buffer
	if err := runSearch([]string{"--regex", "--context", "1", "alpha", "Alpha"}, &stdout); err != nil {
		t.Fatal(err)
	}
	expected := strings.Join([]string{
		"alpha.go-3- // Alpha answers.",
		"alpha.go:4: func Alpha() int {",
		"alpha.go-5- \treturn 42",
	}, "\n") + "\n"
	if stdout.String() != expected {
		t.Fatalf("search output = %q", stdout.String())
	}
}

func TestSearchFilesAndCountModes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"matches": []codeintel.Match{
				{Path: "alpha.go", Line: 4, Preview: "func Alpha() int {"},
				{Path: "alpha.go", Line: 5, Preview: "return 42"},
			},
			"total": 2,
		})
	})
	exploreProject(t, mux)

	var files bytes.Buffer
	if err := runSearch([]string{"--files", "alpha", "Alpha"}, &files); err != nil {
		t.Fatal(err)
	}
	if files.String() != "alpha.go\n" {
		t.Fatalf("--files output = %q", files.String())
	}

	var counts bytes.Buffer
	if err := runSearch([]string{"--count", "alpha", "Alpha"}, &counts); err != nil {
		t.Fatal(err)
	}
	if counts.String() != "alpha.go: 2\ntotal: 2\n" {
		t.Fatalf("--count output = %q", counts.String())
	}
}

func TestReadPrintsNumberedSpanFromTheWorkingTree(t *testing.T) {
	exploreProject(t, nil)

	var stdout bytes.Buffer
	if err := runRead([]string{"--from", "4", "--to", "5", "alpha", "alpha.go"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "     4\tfunc Alpha() int {\n     5\t\treturn 42\n" {
		t.Fatalf("read output = %q", stdout.String())
	}
}

func TestReadRefusesEscapingPaths(t *testing.T) {
	exploreProject(t, nil)

	var stdout bytes.Buffer
	err := runRead([]string{"alpha", "../secrets.txt"}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "escapes the project root") {
		t.Fatalf("escape err = %v", err)
	}
}

func TestOutlineAndSymbolRenderServiceAnswers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /outline", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(codeintel.Outline{
			Path: "alpha.go", Language: "go", Lines: 6,
			Symbols: []codeintel.Symbol{{Name: "Alpha", Kind: "function", StartLine: 4, EndLine: 6, Signature: "func Alpha() int {"}},
			Syntax:  codeintel.Syntax{Reported: true, Valid: true},
		})
	})
	mux.HandleFunc("POST /locate", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(codeintel.Located{
			Name: "Alpha",
			Files: []codeintel.FileOccurrences{{
				Path: "alpha.go",
				Occurrences: []codeintel.Occurrence{
					{Line: 4, Declaration: true, Kind: "function", Preview: "func Alpha() int {"},
				},
			}},
			Declarations: 1,
		})
	})
	exploreProject(t, mux)

	outline := exploreCommand(t, runOutline, "alpha", "alpha.go")
	if !strings.Contains(outline, "alpha.go — go, 6 lines") ||
		!strings.Contains(outline, "function Alpha  4-6  func Alpha() int {") {
		t.Fatalf("outline output = %q", outline)
	}

	symbol := exploreCommand(t, runSymbol, "alpha", "Alpha")
	if !strings.Contains(symbol, "definitions (1):") ||
		!strings.Contains(symbol, "alpha.go:4: func Alpha() int {  [function]") {
		t.Fatalf("symbol output = %q", symbol)
	}
}

func TestMapPrintsBudgetedText(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graph/repository-map", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(codeintel.MapResponse{
			Map: "alpha.go\n  function Alpha\n", Characters: 25, MaxChars: 256, Truncated: true,
		})
	})
	exploreProject(t, mux)

	output := exploreCommand(t, runMap, "--chars", "256", "alpha")
	if !strings.Contains(output, "alpha.go\n  function Alpha") ||
		!strings.Contains(output, "[truncated at 256 characters") {
		t.Fatalf("map output = %q", output)
	}
}

func TestExploreCommandsRequireARunningService(t *testing.T) {
	isolateHome(t)
	dir := makeProjectDir(t, t.TempDir(), "alpha")
	if _, _, err := project(t, "add", dir); err != nil {
		t.Fatal(err)
	}
	useRuntime(t,
		scriptedReply{match: "info --format json", stdout: `{"host":{"os":"linux"}}`},
		scriptedReply{match: "inspect", stdout: "exited||\n"},
	)

	var stdout bytes.Buffer
	err := runSearch([]string{"alpha", "Alpha"}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "lctk project start") {
		t.Fatalf("stopped-service err = %v", err)
	}
}
