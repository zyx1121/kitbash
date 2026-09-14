package mounts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/problem"
)

// tree builds the two roots every case below is checked against: a home
// holding two visible folders and one without a manifest, and an /org holding
// one visible folder. The uid is this test's own, so "owned by the member" is
// a question the temporary tree can answer.
//
// The folders are the ones a member would have: notes and out are visible and
// mountable, hidden carries no manifest and is therefore not, and handbook is
// the shared folder of /org.
func tree(t *testing.T) (home, org string) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "home", "loki")
	org = filepath.Join(base, "org")
	for _, dir := range []string{home, org} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	visible := func(root, name string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
		body := "name: " + name + "\ndescription: >-\n  The folder " + name + ", which a Process may mount.\n"
		if err := os.WriteFile(filepath.Join(dir, "kitbash.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("writing the manifest of %s: %v", dir, err)
		}
	}
	visible(home, "notes")
	visible(home, "out")
	visible(org, "handbook")

	// A folder with no manifest at all, which the surface does not name and a
	// Process therefore cannot mount.
	if err := os.MkdirAll(filepath.Join(home, "hidden"), 0o755); err != nil {
		t.Fatalf("creating the folder without a manifest: %v", err)
	}
	// A file inside a visible folder, for the case that mounts one.
	if err := os.WriteFile(filepath.Join(home, "notes", "today.md"), []byte("# today\n"), 0o644); err != nil {
		t.Fatalf("writing a file in notes: %v", err)
	}
	// A visible folder inside a visible one, so the rule is exercised deeper
	// than the folder that carries the top level manifest.
	visible(filepath.Join(home, "notes"), "week")
	// And an invisible folder inside that same visible one, which is the whole
	// point of the chain: fs_read refuses it, so a Process cannot be given it
	// either. A check that looked only at the top level folder would allow it.
	if err := os.MkdirAll(filepath.Join(home, "notes", "hidden-sub", "deeper"), 0o755); err != nil {
		t.Fatalf("creating the invisible folder below notes: %v", err)
	}
	// A relative link to a visible folder beside it, which escapes nothing and
	// resolves to something the surface would allow. RESOLVE_BENEATH has no
	// opinion on it, because an absolute or upward link is what that flag
	// refuses, so what refuses this one is RESOLVE_NO_SYMLINKS alone.
	if err := os.Symlink("week", filepath.Join(home, "notes", "sideways")); err != nil {
		t.Fatalf("creating the link inside the root: %v", err)
	}
	// A link out of the home, inside a visible folder: the escape this package
	// exists to refuse.
	if err := os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(home, "notes", "away")); err != nil {
		t.Fatalf("creating the escaping link: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "elsewhere"), 0o755); err != nil {
		t.Fatalf("creating the folder the link points at: %v", err)
	}
	// Another member's home beside this one, which is what makes "that is
	// another member's home" a different answer from "that is not a root".
	other := filepath.Join(base, "home", "kilo")
	if err := os.MkdirAll(filepath.Join(other, "notes"), 0o755); err != nil {
		t.Fatalf("creating the other member's home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(other, "notes", "kitbash.yaml"),
		[]byte("name: notes\ndescription: >-\n  Another member's notes.\n"), 0o644); err != nil {
		t.Fatalf("writing the other member's manifest: %v", err)
	}
	return home, org
}

// checker is the member's own checker over that tree. The uid is this process's
// own, so every folder it just created is owned by the member.
func checker(t *testing.T, home, org string) *mounts.Checker {
	t.Helper()
	return &mounts.Checker{
		Owner: "loki",
		Home:  home,
		Org:   org,
		UID:   os.Getuid(),
		Homes: filepath.Dir(home),
	}
}

// TestResolveOne is the whole rule set, one case per rule, because this is the
// function that decides what a container can read and write.
func TestResolveOne(t *testing.T) {
	home, org := tree(t)
	c := checker(t, home, org)
	base := filepath.Dir(filepath.Dir(home))

	for _, tc := range []struct {
		name   string
		mount  mounts.Declared
		slug   string // empty means it is allowed
		source string // the resolved source, for the allowed cases
	}{
		{
			name:   "a folder of the owner's home read only",
			mount:  mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: "ro"},
			source: filepath.Join(home, "notes"),
		},
		{
			name:   "a folder of the owner's home read write",
			mount:  mounts.Declared{Source: filepath.Join(home, "out"), Target: "/files/out", Mode: "rw"},
			source: filepath.Join(home, "out"),
		},
		{
			name:   "a visible folder below a visible top level folder",
			mount:  mounts.Declared{Source: filepath.Join(home, "notes", "week"), Target: "/files/week"},
			source: filepath.Join(home, "notes", "week"),
		},
		{
			// The reproduction of the hole this rule closes: the top level
			// folder is visible and this one is not, and fs_read of it is
			// not-visible, so a mount of it has to be too.
			name:  "an invisible folder below a visible top level folder",
			mount: mounts.Declared{Source: filepath.Join(home, "notes", "hidden-sub"), Target: "/files/sub"},
			slug:  problem.SlugNotVisible,
		},
		{
			name:  "a folder below an invisible one",
			mount: mounts.Declared{Source: filepath.Join(home, "notes", "hidden-sub", "deeper"), Target: "/files/deeper"},
			slug:  problem.SlugNotVisible,
		},
		{
			// A relative link to a visible folder beside it: nothing escapes
			// the root, so RESOLVE_BENEATH has nothing to say about it, and
			// what it resolves to is a folder a mount would otherwise be given.
			// It is refused by RESOLVE_NO_SYMLINKS alone, which is the flag
			// this case pins: kitbash follows no link, and the target of a link
			// is not the folder the manifest named.
			name:  "a link that stays inside the root",
			mount: mounts.Declared{Source: filepath.Join(home, "notes", "sideways"), Target: "/files/sideways"},
			slug:  problem.SlugInvalidPath,
		},
		{
			name:   "a top level folder of /org read only",
			mount:  mounts.Declared{Source: filepath.Join(org, "handbook"), Target: "/files/handbook"},
			source: filepath.Join(org, "handbook"),
		},
		{
			name:  "another member's home",
			mount: mounts.Declared{Source: filepath.Join(base, "home", "kilo", "notes"), Target: "/files/theirs"},
			slug:  problem.SlugNotPermitted,
		},
		{
			name:  "/org read write",
			mount: mounts.Declared{Source: filepath.Join(org, "handbook"), Target: "/files/handbook", Mode: "rw"},
			slug:  problem.SlugNotPermitted,
		},
		{
			name:  "a source outside both roots",
			mount: mounts.Declared{Source: "/var/lib/secrets", Target: "/files/secrets"},
			slug:  problem.SlugNotPermitted,
		},
		{
			name:  "a symbolic link out of the home",
			mount: mounts.Declared{Source: filepath.Join(home, "notes", "away"), Target: "/files/away"},
			slug:  problem.SlugInvalidPath,
		},
		{
			// Written out rather than joined, because filepath.Join would
			// clean the segment away and this is the path a manifest can
			// carry: the refusal has to happen on the string a caller sent.
			name:  "a parent reference",
			mount: mounts.Declared{Source: home + "/notes/../../kilo/notes", Target: "/files/up"},
			slug:  problem.SlugInvalidPath,
		},
		{
			name:  "a file rather than a folder",
			mount: mounts.Declared{Source: filepath.Join(home, "notes", "today.md"), Target: "/files/today"},
			slug:  problem.SlugInvalidPath,
		},
		{
			name:  "a folder without a manifest",
			mount: mounts.Declared{Source: filepath.Join(home, "hidden"), Target: "/files/hidden"},
			slug:  problem.SlugNotVisible,
		},
		{
			name:  "the root itself",
			mount: mounts.Declared{Source: home, Target: "/files/home"},
			slug:  problem.SlugNotVisible,
		},
		{
			name:  "a target under /etc",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/etc/notes"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a target under /proc",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/proc/self"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a target under /usr",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/usr/local/share"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a target of /bin",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/bin"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a target under /lib64",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/lib64/x"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a relative target",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "files/notes"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a mode that is neither ro nor rw",
			mount: mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: "write"},
			slug:  problem.SlugBadRequest,
		},
		{
			name:  "a source that does not exist",
			mount: mounts.Declared{Source: filepath.Join(home, "notes", "nothing"), Target: "/files/nothing"},
			slug:  problem.SlugNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, prob := c.ResolveOne("/home/loki/reader", tc.mount)
			if tc.slug == "" {
				if prob != nil {
					t.Fatalf("this mount is refused with %s: %s", prob.Slug(), prob.Detail)
				}
				if got.Source != tc.source {
					t.Errorf("the source resolved to %s, want %s", got.Source, tc.source)
				}
				if got.Target != filepath.Clean(tc.mount.Target) {
					t.Errorf("the target is %s, want %s", got.Target, tc.mount.Target)
				}
				want := tc.mount.Mode
				if want == "" {
					want = mounts.ModeRO
				}
				if got.Mode != want {
					t.Errorf("the mode is %s, want %s", got.Mode, want)
				}
				return
			}
			if prob == nil {
				t.Fatalf("this mount is allowed and resolved to %+v, want %s", got, tc.slug)
			}
			if prob.Slug() != tc.slug {
				t.Errorf("this mount is refused with %s (%s), want %s", prob.Slug(), prob.Detail, tc.slug)
			}
			if prob.Fix == "" {
				t.Errorf("the refusal carries no fix: %s", prob.Detail)
			}
		})
	}
}

// An admin is refused a rw mount of /org exactly like a member: an approval is
// the trail of every write there, and an admin with a rw mount would be a way
// past it that leaves no record, see PLAN.md section 2.1. The checker carries
// no admin flag at all, which is the shape of that decision, so this is the
// same call made by a member of kitbash-admin.
func TestOrgIsReadOnlyForAdminsToo(t *testing.T) {
	home, org := tree(t)
	admin := checker(t, home, org)
	admin.Owner = "admin1"
	_, prob := admin.ResolveOne("/home/admin1/reader", mounts.Declared{
		Source: filepath.Join(org, "handbook"), Target: "/files/handbook", Mode: "rw",
	})
	if prob == nil || prob.Slug() != problem.SlugNotPermitted {
		t.Fatalf("an admin was given a rw mount of /org: %+v", prob)
	}
	if !strings.Contains(prob.Detail, "administrators included") {
		t.Errorf("the refusal is %q, want it to say administrators are included", prob.Detail)
	}
}

// A source under the home that belongs to somebody else is refused: a home
// holds folders another member shared through a Linux group, and mounting one
// of those hands a Process what its owner was given to read rather than what
// they own.
func TestASourceTheOwnerDoesNotOwnIsRefused(t *testing.T) {
	home, org := tree(t)
	c := checker(t, home, org)
	// Nobody is the uid this member is not. The folder itself is unchanged, so
	// the only thing that differs from the allowed case above is who owns it.
	c.UID = os.Getuid() + 1
	_, prob := c.ResolveOne("/home/loki/reader", mounts.Declared{
		Source: filepath.Join(home, "notes"), Target: "/files/notes",
	})
	if prob == nil || prob.Slug() != problem.SlugNotPermitted {
		t.Fatalf("a folder the member does not own was mounted: %+v", prob)
	}
}

// More than four mounts is refused before any of them is resolved: the limit
// is what keeps one manifest from turning a start into a hundred resolutions.
func TestMoreThanFourMountsIsRefused(t *testing.T) {
	home, org := tree(t)
	c := checker(t, home, org)
	var declared []mounts.Declared
	for i := 0; i < mounts.Max+1; i++ {
		declared = append(declared, mounts.Declared{
			Source: filepath.Join(home, "notes"),
			Target: filepath.Join("/files", "notes"+string(rune('a'+i))),
		})
	}
	_, prob := c.Resolve("/home/loki/reader", declared)
	if prob == nil || prob.Slug() != problem.SlugBadRequest {
		t.Fatalf("five mounts were accepted: %+v", prob)
	}
}

// Four are not, and they come back in the order they were declared: a unit
// reads its own mounts in the order it wrote them.
func TestFourMountsResolveInOrder(t *testing.T) {
	home, org := tree(t)
	c := checker(t, home, org)
	resolved, prob := c.Resolve("/home/loki/reader", []mounts.Declared{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes"},
		{Source: filepath.Join(home, "out"), Target: "/files/out", Mode: "rw"},
		{Source: filepath.Join(org, "handbook"), Target: "/files/handbook"},
		{Source: filepath.Join(home, "notes", "week"), Target: "/files/week"},
	})
	if prob != nil {
		t.Fatalf("four legal mounts were refused: %s", prob.Detail)
	}
	want := []string{"/files/notes", "/files/out", "/files/handbook", "/files/week"}
	for i, target := range want {
		if resolved[i].Target != target {
			t.Errorf("mount %d is %s, want %s", i, resolved[i].Target, target)
		}
	}
	if resolved[1].Mode != mounts.ModeRW || resolved[0].ReadOnly() != true {
		t.Errorf("the modes came back as %+v, want the second one read write", resolved)
	}
}

// A unit with no mounts resolves to nothing, which is every Package written
// before this existed.
func TestNoMountsResolveToNothing(t *testing.T) {
	home, org := tree(t)
	resolved, prob := checker(t, home, org).Resolve("/home/loki/echo", nil)
	if prob != nil || resolved != nil {
		t.Fatalf("a unit with no mounts resolved to %+v, %v", resolved, prob)
	}
}

// Podman is the only form of a mount that leaves kitbash, so the mode has to
// survive the translation: an ro mount rendered as rw is a container writing
// into a member's home.
func TestPodmanCarriesTheMode(t *testing.T) {
	got := mounts.Podman([]mounts.Resolved{
		{Source: "/home/loki/notes", Target: "/files/notes", Mode: mounts.ModeRO},
		{Source: "/home/loki/out", Target: "/files/out", Mode: mounts.ModeRW},
	})
	if len(got) != 2 || !got[0].ReadOnly || got[1].ReadOnly {
		t.Fatalf("Podman answered %+v, want the first read only and the second not", got)
	}
	if got[0].Source != "/home/loki/notes" || got[1].Target != "/files/out" {
		t.Errorf("Podman answered %+v, want the paths it was given", got)
	}
}
