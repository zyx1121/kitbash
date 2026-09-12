package fs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
)

// An SSH session is a member connecting through ForceCommand, who must not be
// able to choose the roots.
func TestRootsEnvIsIgnoredInAnSSHSession(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv(fs.RootsEnv, elsewhere)
	t.Setenv(fs.SSHEnv, "10.0.0.2 51000 10.0.0.1 22")

	service, err := fs.NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	for _, root := range service.Roots() {
		if root == filepath.Clean(elsewhere) {
			t.Fatalf("KITBASH_ROOTS was honoured in an SSH session: %v", service.Roots())
		}
	}
	if service.Roots()[0] != fs.OrgRoot {
		t.Errorf("the first root is %q, want %s", service.Roots()[0], fs.OrgRoot)
	}
}

// On a kitbash host /org is the shared root without anyone saying so, and the
// override that names another one is refused to a member the same way the
// roots override is.
func TestTheSharedRootIsOrgAndTheOverrideIsIgnoredInAnSSHSession(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv(fs.SSHEnv, "10.0.0.2 51000 10.0.0.1 22")
	t.Setenv(fs.SharedEnv, elsewhere)

	service, err := fs.NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if service.Shared() != fs.OrgRoot {
		t.Errorf("the shared root is %q, want %s", service.Shared(), fs.OrgRoot)
	}
}

func TestSharedEnvAppliesOutsideAnSSHSession(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv(fs.SSHEnv, "")
	t.Setenv(fs.RootsEnv, elsewhere)
	t.Setenv(fs.SharedEnv, elsewhere)

	service, err := fs.NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if service.Shared() != filepath.Clean(elsewhere) {
		t.Errorf("the shared root is %q, want %s", service.Shared(), elsewhere)
	}
}

func TestRootsEnvAppliesOutsideAnSSHSession(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv(fs.SSHEnv, "")
	t.Setenv(fs.RootsEnv, elsewhere)

	service, err := fs.NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if len(service.Roots()) != 1 || service.Roots()[0] != filepath.Clean(elsewhere) {
		t.Errorf("roots are %v, want %s", service.Roots(), elsewhere)
	}
	if service.User() == "" {
		t.Error("the service has no user to attribute commits to")
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("the fixture root vanished: %v", err)
	}
}
