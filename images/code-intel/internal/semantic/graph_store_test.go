package semantic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/images/code-intel/internal/symbols"
)

func TestGraphPublishesQueriesAndRetractsWithSemanticGeneration(t *testing.T) {
	ctx := context.Background()
	source := &sourceStub{files: map[string][]byte{
		"src/main.js": []byte("import { Work } from './dep.js';\nfunction Run(){ Work(); }\n"),
		"src/dep.js":  []byte("export function Work(){ return 1; }\n"),
	}}
	engine, err := symbols.New()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	embedder := &deterministicEmbedder{dimension: 16}
	store, err := Open(Config{Path: filepath.Join(t.TempDir(), "semantic.db"), Model: "test", Dimensions: 16}, source, engine, embedder)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Sync(ctx, exactState(1, source)); err != nil {
		t.Fatalf("Sync generation 1: %v", err)
	}
	status, err := store.GraphStatus()
	if err != nil || !status.Ready || status.Generation != 1 || status.FileCount != 2 || status.CallCount != 1 {
		t.Fatalf("graph status = %+v, err = %v", status, err)
	}
	callers, err := store.Callers(ctx, GraphRequest{Name: "Work", Limit: 1})
	if err != nil || len(callers.Matches) != 1 || callers.Matches[0].Caller != "Run" {
		t.Fatalf("callers = %+v, err = %v", callers, err)
	}
	callees, err := store.Callees(ctx, GraphRequest{Name: "Run"})
	if err != nil || len(callees.Matches) != 1 || callees.Matches[0].Callee != "Work" {
		t.Fatalf("callees = %+v, err = %v", callees, err)
	}
	dependency, err := store.DependencyPath(ctx, DependencyRequest{From: "src/main.js", To: "src/dep.js"})
	if err != nil || !dependency.Found || len(dependency.Path) != 2 {
		t.Fatalf("dependency = %+v, err = %v", dependency, err)
	}
	impact, err := store.Impact(ctx, ImpactRequest{Target: "Work"})
	if err != nil || len(impact.Calls) != 1 || impact.Files[0] != "src/main.js" {
		t.Fatalf("impact = %+v, err = %v", impact, err)
	}
	repositoryMap, err := store.RepositoryMap(ctx, MapRequest{MaxChars: 2048})
	if err != nil || !strings.Contains(repositoryMap.Map, "src/main.js") || !strings.Contains(repositoryMap.Map, "function Run") {
		t.Fatalf("repository map = %+v, err = %v", repositoryMap, err)
	}

	source.files["src/main.js"] = []byte("import { Work } from './dep.js';\nfunction Run(){ Other(); }\n")
	if _, err := store.Sync(ctx, exactState(2, source)); err != nil {
		t.Fatalf("Sync generation 2: %v", err)
	}
	callers, err = store.Callers(ctx, GraphRequest{Name: "Work"})
	if err != nil || callers.Total != 0 {
		t.Fatalf("retracted callers = %+v, err = %v", callers, err)
	}
	delete(source.files, "src/dep.js")
	if _, err := store.Sync(ctx, exactState(3, source)); err != nil {
		t.Fatalf("Sync generation 3: %v", err)
	}
	status, _ = store.GraphStatus()
	if status.FileCount != 1 || status.Generation != 3 {
		t.Fatalf("post-delete graph status = %+v", status)
	}
}

func TestSchemaOneMigratesWithoutDiscardingSemanticState(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "semantic.db")
	source := &sourceStub{files: map[string][]byte{"a.go": []byte("package a\nfunc A(){}\n")}}
	engine, err := symbols.New()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	embedder := &deterministicEmbedder{dimension: 16}
	store, err := Open(Config{Path: database, Model: "test", Dimensions: 16}, source, engine, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sync(ctx, exactState(1, source)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE graph_calls; DROP TABLE graph_imports; DROP TABLE graph_nodes; DROP TABLE graph_files; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Config{Path: database, Model: "test", Dimensions: 16}, source, engine, embedder)
	if err != nil {
		t.Fatalf("migrate schema 1: %v", err)
	}
	defer reopened.Close()
	semanticStatus, err := reopened.Status()
	if err != nil || semanticStatus.Generation != 1 || semanticStatus.ChunkCount == 0 {
		t.Fatalf("semantic state after migration = %+v, err = %v", semanticStatus, err)
	}
	if _, err := reopened.Sync(ctx, exactState(1, source)); err != nil {
		t.Fatalf("rebuild graph after migration: %v", err)
	}
	graphStatus, err := reopened.GraphStatus()
	if err != nil || !graphStatus.Ready || graphStatus.Generation != 1 || graphStatus.NodeCount == 0 {
		t.Fatalf("graph after migration = %+v, err = %v", graphStatus, err)
	}
	activeInfo, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}
	validatedInfo, err := os.Stat(database + ".validated-v2")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(activeInfo, validatedInfo) {
		t.Fatal("successful migration validation marker does not name the active database inode")
	}
}

func TestActivatedMigrationRequiresValidationAfterRestart(t *testing.T) {
	database := filepath.Join(t.TempDir(), "semantic.db")
	source := &sourceStub{files: map[string][]byte{"a.go": []byte("package a\nfunc A(){}\n")}}
	embedder := &deterministicEmbedder{dimension: 16}
	store, err := Open(Config{Path: database, Model: "model-a", Dimensions: 16}, source, outlineStub{}, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE graph_calls; DROP TABLE graph_imports; DROP TABLE graph_nodes; DROP TABLE graph_files; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rollback, err := prepareDatabaseMigration(database)
	if err != nil || rollback == "" {
		t.Fatalf("activate migration rollback=%q err=%v", rollback, err)
	}
	retriedRollback, err := prepareDatabaseMigration(database)
	if err != nil || retriedRollback != rollback {
		t.Fatalf("recover unvalidated activation rollback=%q err=%v", retriedRollback, err)
	}
	if err := os.Link(database, database+".validated-v2.pending"); err != nil {
		t.Fatal(err)
	}
	committedRollback, err := prepareDatabaseMigration(database)
	if err != nil || committedRollback != "" {
		t.Fatalf("recover committed validation rollback=%q err=%v", committedRollback, err)
	}
	activeInfo, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}
	validatedInfo, err := os.Stat(database + ".validated-v2")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(activeInfo, validatedInfo) {
		t.Fatal("recovered validation marker does not name the active database inode")
	}
}

func TestFailedSchemaOneActivationRestoresOriginalDatabase(t *testing.T) {
	database := filepath.Join(t.TempDir(), "semantic.db")
	source := &sourceStub{files: map[string][]byte{"a.go": []byte("package a\nfunc A(){}\n")}}
	embedder := &deterministicEmbedder{dimension: 16}
	store, err := Open(Config{Path: database, Model: "model-a", Dimensions: 16}, source, outlineStub{}, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE graph_calls; DROP TABLE graph_imports; DROP TABLE graph_nodes; DROP TABLE graph_files; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		_, err = Open(Config{Path: database, Model: "model-b", Dimensions: 16}, source, outlineStub{}, embedder)
		var typed *Error
		if !errors.As(err, &typed) || typed.Code != CodeModelMismatch {
			t.Fatalf("Open incompatible migration attempt %d error = %v, want MODEL_MISMATCH", attempt, err)
		}
		if _, err := os.Stat(database + ".failed-v2"); err != nil {
			t.Fatalf("failed migrated database was not preserved on attempt %d: %v", attempt, err)
		}

		legacy, err := openMigrationDatabase(database)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := legacy.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			legacy.Close()
			t.Fatal(err)
		}
		if err := legacy.Close(); err != nil {
			t.Fatal(err)
		}
		if version != 1 {
			t.Fatalf("restored database schema after attempt %d = %d, want 1", attempt, version)
		}
	}
}

func TestInterruptedRestoreCompletesAtomicReplacement(t *testing.T) {
	for _, test := range []struct {
		name    string
		pending bool
	}{
		{name: "committed diagnostic"},
		{name: "pending diagnostic replaces old evidence", pending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			database := filepath.Join(directory, "semantic.db")
			rollback := database + ".rollback-v1"
			failed := database + ".failed-v2"
			if err := os.WriteFile(database, []byte("schema-v2"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rollback, []byte("schema-v1"), 0o600); err != nil {
				t.Fatal(err)
			}
			diagnosticPath := failed
			if test.pending {
				if err := os.WriteFile(failed, []byte("old-evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
				diagnosticPath += ".pending"
			}
			if err := os.Link(database, diagnosticPath); err != nil {
				t.Fatal(err)
			}

			if err := recoverInterruptedRestore(database, rollback, failed); err != nil {
				t.Fatalf("recover interrupted restore: %v", err)
			}
			active, err := os.ReadFile(database)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic, err := os.ReadFile(failed)
			if err != nil {
				t.Fatal(err)
			}
			if string(active) != "schema-v1" || string(diagnostic) != "schema-v2" {
				t.Fatalf("active=%q diagnostic=%q", active, diagnostic)
			}
			if _, err := os.Stat(rollback); !os.IsNotExist(err) {
				t.Fatalf("consumed rollback remains: %v", err)
			}
			if _, err := os.Stat(failed + ".pending"); !os.IsNotExist(err) {
				t.Fatalf("pending diagnostic remains: %v", err)
			}
		})
	}
}

func TestInterruptedPreCommitMigrationRetriesWithoutLosingTheActiveDatabase(t *testing.T) {
	database := filepath.Join(t.TempDir(), "semantic.db")
	source := &sourceStub{files: map[string][]byte{"a.go": []byte("package a\nfunc A(){}\n")}}
	embedder := &deterministicEmbedder{dimension: 16}
	store, err := Open(Config{Path: database, Model: "model-a", Dimensions: 16}, source, outlineStub{}, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE graph_calls; DROP TABLE graph_imports; DROP TABLE graph_nodes; DROP TABLE graph_files; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// This is the only pre-commit crash state after the durable migration copy
	// exists and the original v1 inode has received its rollback name.
	if err := copyDatabaseFile(database, database+".migration"); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(database, database+".rollback-v1"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Config{Path: database, Model: "model-a", Dimensions: 16}, source, outlineStub{}, embedder)
	if err != nil {
		t.Fatalf("retry interrupted migration: %v", err)
	}
	defer reopened.Close()
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("retried migration schema = %d, want %d", version, schemaVersion)
	}
	if _, err := os.Stat(database + ".migration"); !os.IsNotExist(err) {
		t.Fatalf("interrupted migration copy remains: %v", err)
	}
	activeInfo, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}
	rollbackInfo, err := os.Stat(database + ".rollback-v1")
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(activeInfo, rollbackInfo) {
		t.Fatal("migration did not atomically replace the active v1 inode")
	}
}

func TestCallPaginationAndDuplicateDeclarationAmbiguity(t *testing.T) {
	ctx := context.Background()
	source := &sourceStub{files: map[string][]byte{
		"one.js": []byte("function One(){ Work(); }\n"),
		"two.js": []byte("function Two(){ Work(); }\n"),
		"dep.js": []byte("export function Work(){}\n"),
		"dep.ts": []byte("export function Work(): void {}\n"),
	}}
	engine, err := symbols.New()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	store, err := Open(Config{Path: filepath.Join(t.TempDir(), "semantic.db"), Model: "test", Dimensions: 16},
		source, engine, &deterministicEmbedder{dimension: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Sync(ctx, exactState(1, source)); err != nil {
		t.Fatal(err)
	}
	first, err := store.Callers(ctx, GraphRequest{Name: "Work", Limit: 1})
	if err != nil || !first.Ambiguous || first.Declarations != 2 || !first.Truncated || first.NextCursor == "" || len(first.Matches) != 1 {
		t.Fatalf("first page = %+v, err = %v", first, err)
	}
	second, err := store.Callers(ctx, GraphRequest{Name: "Work", Limit: 1, Cursor: first.NextCursor})
	if err != nil || second.Truncated || len(second.Matches) != 1 || second.Matches[0].Path == first.Matches[0].Path {
		t.Fatalf("second page = %+v, err = %v", second, err)
	}
}

func TestDependencyGraphResolvesGoModulePathImports(t *testing.T) {
	ctx := context.Background()
	source := &sourceStub{files: map[string][]byte{
		"go.mod": []byte("module example.com/app\n\ngo 1.25\n"),
		"main.go": []byte("package main\n\nimport (\n\t\"strings\"\n\n\t\"example.com/app/lib/v2/core\"\n" +
			"\t\"example.com/app/util\"\n\t\"example.com/unknown/dep\"\n)\n\nfunc Run() { util.Work() }\n"),
		"strings.go":         []byte("package main\n"),
		"util/util.go":       []byte("package util\n\nfunc Work() {}\n"),
		"util/extra.go":      []byte("package util\n"),
		"modules/lib/go.mod": []byte("module example.com/app/lib/v2\n\ngo 1.25\n"),
		"modules/lib/entry.go": []byte("package lib\n\nimport (\n\t\"example.com/app\"\n" +
			"\t\"example.com/app/lib/v2/core\"\n)\n"),
		"modules/lib/core/core.go": []byte("package core\n"),
	}}
	engine, err := symbols.New()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	store, err := Open(Config{Path: filepath.Join(t.TempDir(), "semantic.db"), Model: "test", Dimensions: 16},
		source, engine, &deterministicEmbedder{dimension: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Sync(ctx, exactState(1, source)); err != nil {
		t.Fatal(err)
	}
	graph, _, err := store.dependencyGraph(ctx)
	if err != nil {
		t.Fatalf("dependencyGraph: %v", err)
	}
	expected := map[string][]string{
		// The root-module import fans out to every Go file in util/. The nested
		// module's /v2 path wins longest-prefix matching, so its import lands in
		// modules/lib/core/ instead of the root-relative lib/v2/core/. The unknown
		// module and the stdlib "strings" import (with a sibling strings.go
		// present) resolve to nothing.
		"main.go": {"modules/lib/core/core.go", "util/extra.go", "util/util.go"},
		// A nested-module file imports through its own module path, and the exact
		// root module path names the workspace root package without go.mod.
		"modules/lib/entry.go": {"main.go", "modules/lib/core/core.go", "strings.go"},
	}
	if !reflect.DeepEqual(graph, expected) {
		t.Fatalf("dependency graph = %+v, want %+v", graph, expected)
	}
}

func TestGoModulePathParsesOnlyTheModuleDirective(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{"plain directive", "module example.com/app\n\ngo 1.25\n", "example.com/app"},
		{"comments and trailing comment", "// module noise\nmodule example.com/app // v1\n", "example.com/app"},
		{"quoted path", "module \"example.com/app\"\n", "example.com/app"},
		{"major version suffix", "module example.com/app/v2\n", "example.com/app/v2"},
		{"prefixed word is not the directive", "modules example.com/app\n", ""},
		{"missing path", "module\ngo 1.25\n", ""},
		{"malformed path", "module bad path\n", ""},
		{"empty file", "", ""},
	} {
		if got := goModulePath([]byte(test.content)); got != test.want {
			t.Fatalf("%s: goModulePath = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestGoWorkspaceGraphToolsAnswerSanely(t *testing.T) {
	ctx := context.Background()
	source := &sourceStub{files: map[string][]byte{
		"go.mod":                []byte("module example.com/tool\n\ngo 1.25\n"),
		"cmd/tool/main.go":      []byte("package main\n\nimport \"example.com/tool/internal/core\"\n\nfunc main() { core.Run() }\n"),
		"internal/core/core.go": []byte("package core\n\nfunc Run() { helper() }\n\nfunc helper() {}\n"),
	}}
	engine, err := symbols.New()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	store, err := Open(Config{Path: filepath.Join(t.TempDir(), "semantic.db"), Model: "test", Dimensions: 16},
		source, engine, &deterministicEmbedder{dimension: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Sync(ctx, exactState(1, source)); err != nil {
		t.Fatal(err)
	}
	dependency, err := store.DependencyPath(ctx, DependencyRequest{From: "cmd/tool/main.go", To: "internal/core/core.go"})
	if err != nil || !dependency.Found ||
		!reflect.DeepEqual(dependency.Path, []string{"cmd/tool/main.go", "internal/core/core.go"}) {
		t.Fatalf("dependency = %+v, err = %v", dependency, err)
	}
	impact, err := store.Impact(ctx, ImpactRequest{Target: "internal/core/core.go"})
	if err != nil || !reflect.DeepEqual(impact.Files, []string{"cmd/tool/main.go"}) {
		t.Fatalf("impact = %+v, err = %v", impact, err)
	}
	repositoryMap, err := store.RepositoryMap(ctx, MapRequest{MaxChars: 4096})
	if err != nil || !strings.Contains(repositoryMap.Map, "cmd/tool/main.go") ||
		!strings.Contains(repositoryMap.Map, "internal/core/core.go") ||
		!strings.Contains(repositoryMap.Map, "Run") {
		t.Fatalf("repository map = %+v, err = %v", repositoryMap, err)
	}
}

func TestRepositoryMapBudgetCountsUnicodeCharacters(t *testing.T) {
	var builder strings.Builder
	used := 0
	if !appendMapText(&builder, &used, 3, "é界a") {
		t.Fatal("three Unicode characters did not fit a three-character budget")
	}
	if appendMapText(&builder, &used, 3, "b") {
		t.Fatal("text exceeded the exact character budget")
	}
	if used != 3 || builder.String() != "é界a" {
		t.Fatalf("used=%d map=%q", used, builder.String())
	}
}
