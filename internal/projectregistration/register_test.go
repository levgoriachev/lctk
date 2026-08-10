package projectregistration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

func TestRegisterPersistsTheAuthoritativePathWithoutAuthorizingAClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LCTK_HOME", home)
	projectDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := Register(projectDir, projectregistry.ProfileFull)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := projectregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	project, err := loaded.Resolve(result.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.Path != result.Project.Path || project.Profile != projectregistry.ProfileFull {
		t.Fatalf("project=%+v", project)
	}
	if _, err := os.Stat(filepath.Join(home, "oauth.json")); !os.IsNotExist(err) {
		t.Fatalf("registration must not create OAuth authority: %v", err)
	}
}
