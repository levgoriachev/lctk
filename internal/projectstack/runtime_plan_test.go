package projectstack

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(lctkhome.EnvOverride, home)
	return home
}

func absPath(parts ...string) string {
	joined := filepath.Join(parts...)
	if runtime.GOOS == "windows" {
		return filepath.Join(`C:\`, joined)
	}
	return filepath.Join("/", joined)
}

var testBudget = hostsettings.Resources{Mode: hostsettings.ModeNormal}.Budget()

func testProject(id, path string) projectregistry.Project {
	return projectregistry.Project{
		ID:           id,
		Name:         filepath.Base(path),
		Path:         path,
		Key:          strings.ToLower(filepath.ToSlash(path)),
		Profile:      projectregistry.ProfileMinimal,
		RegisteredAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}
}

func TestDeriveNamesIsStableAndPrefixed(t *testing.T) {
	names, err := DeriveNames("alpha-abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	if names.Network != "lctk-alpha-abcd1234-net" || names.Volume != "lctk-alpha-abcd1234-state" ||
		names.ContainerName != "lctk-alpha-abcd1234-code-intel" {
		t.Fatalf("unexpected names: %+v", names)
	}
	if names.Image != ImageRepository+":"+buildinfo.Version {
		t.Errorf("image = %q", names.Image)
	}
	again, err := DeriveNames("alpha-abcd1234")
	if err != nil || again != names {
		t.Fatalf("DeriveNames is not deterministic: %+v, %v", again, err)
	}
}

func TestDeriveNamesRejectsUnusableIdentifiers(t *testing.T) {
	for _, id := range []string{"", "Upper", "has space", "-leading", "semi;colon", "../escape"} {
		if _, err := DeriveNames(id); !errors.Is(err, ErrInvalidProject) {
			t.Errorf("DeriveNames(%q) = %v, want ErrInvalidProject", id, err)
		}
	}
}

func TestRuntimePlanIsDeterministicAndComplete(t *testing.T) {
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))
	first, err := BuildRuntimePlan(project, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildRuntimePlan(project, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, err := RenderRuntimePlan(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := RenderRuntimePlan(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBody) != string(secondBody) {
		t.Fatalf("runtime plan is not deterministic:\n%s\n%s", firstBody, secondBody)
	}
	if first.ProjectID != project.ID || first.Image != ImageRepository+":"+buildinfo.Version ||
		first.Network == "" || first.Volume == "" || first.Container == "" {
		t.Fatalf("runtime plan is incomplete: %+v", first)
	}
	wantSource, err := containerruntime.HostPath(project.Path)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkspaceSource != wantSource {
		t.Errorf("workspace source = %q, want %q", first.WorkspaceSource, wantSource)
	}
	arguments := strings.Join(first.Arguments(), "\n")
	for _, required := range []string{
		"--replace", first.WorkspaceSource + ":" + WorkspaceMount + ":ro",
		first.Volume + ":" + StateMount,
		inference.ProjectEndpoint, "tech.lctk.project-id=" + project.ID,
	} {
		if !strings.Contains(arguments, required) {
			t.Errorf("runtime arguments omit %q:\n%s", required, arguments)
		}
	}
}

func TestRuntimePlanRejectsAnUnusableProject(t *testing.T) {
	if _, err := BuildRuntimePlan(testProject("alpha-abcd1234", ""), testBudget); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("empty path: %v", err)
	}
	if _, err := BuildRuntimePlan(testProject("alpha-abcd1234", "relative/path"), testBudget); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("relative path: %v", err)
	}
	if _, err := BuildRuntimePlan(testProject("Bad ID", absPath("work", "alpha")), testBudget); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("bad id: %v", err)
	}
}

func TestTwoProjectsGetSeparateResources(t *testing.T) {
	alpha, err := BuildRuntimePlan(testProject("alpha-aaaaaaaa", absPath("work", "alpha")), testBudget)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := BuildRuntimePlan(testProject("beta-bbbbbbbb", absPath("work", "beta")), testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Network == beta.Network || alpha.Volume == beta.Volume || alpha.Container == beta.Container {
		t.Fatalf("projects share isolated resources: alpha=%+v beta=%+v", alpha, beta)
	}
	if alpha.Image != beta.Image {
		t.Error("projects do not share the reusable versioned image")
	}
}

func TestWriteRuntimePlanIsAtomicAndPrivate(t *testing.T) {
	home := isolate(t)
	plan, err := BuildRuntimePlan(testProject("alpha-abcd1234", absPath("work", "alpha")), testBudget)
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteRuntimePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, home) || filepath.Base(path) != "runtime.json" {
		t.Fatalf("runtime plan path = %q", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RuntimePlan
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.ProjectID != plan.ProjectID {
		t.Fatalf("written runtime plan is invalid: %v, %+v", err, decoded)
	}
	for index := 0; index < 3; index++ {
		if _, err := WriteRuntimePlan(plan); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "runtime.json" {
		t.Fatalf("unexpected derived state: %+v", entries)
	}
}

func TestTheResourceModeReachesTheRuntimePlan(t *testing.T) {
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))
	quiet, err := BuildRuntimePlan(project, hostsettings.Resources{Mode: hostsettings.ModeQuiet}.Budget())
	if err != nil {
		t.Fatal(err)
	}
	if quiet.CPUs != 1 || !containsString(quiet.Environment, "LCTK_INDEX_PARALLELISM=1") {
		t.Fatalf("quiet budget did not reach runtime plan: %+v", quiet)
	}
	fast, err := BuildRuntimePlan(project, hostsettings.Resources{Mode: hostsettings.ModeFast}.Budget())
	if err != nil {
		t.Fatal(err)
	}
	if fast.CPUs != 0 || containsPrefix(fast.Environment, "LCTK_INDEX_PARALLELISM=") {
		t.Fatalf("fast budget retained a cap: %+v", fast)
	}
	capped, err := BuildRuntimePlan(project, hostsettings.Resources{Mode: hostsettings.ModeNormal, MemoryLimitMB: 1536}.Budget())
	if err != nil {
		t.Fatal(err)
	}
	if capped.MemoryMB != 1536 || !containsString(capped.Arguments(), "1536m") {
		t.Fatalf("memory cap did not reach runtime arguments: %+v", capped)
	}
}

func TestStackDirRejectsAnUnusableIdentifier(t *testing.T) {
	isolate(t)
	for _, id := range []string{"", "..", "../escape", "Upper"} {
		if _, err := StackDir(id); !errors.Is(err, ErrInvalidProject) {
			t.Errorf("StackDir(%q) = %v, want ErrInvalidProject", id, err)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsPrefix(values []string, want string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, want) {
			return true
		}
	}
	return false
}
