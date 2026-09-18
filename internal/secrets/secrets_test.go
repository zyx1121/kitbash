package secrets_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/secrets"
)

// store is one secrets tree in a directory of the test's own, which is what
// the base directory being configurable is for: a unit test never writes to
// /var/lib/kitbash.
func store(t *testing.T) (*secrets.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets")
	s := secrets.New(dir)
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return s, dir
}

// The whole of what a member does with one: set it, see the name, read the
// value back, set it again and read the new one, drop it.
func TestSetListGetRemove(t *testing.T) {
	s, _ := store(t)

	updated, err := s.Set("alice", "ANTHROPIC_API_KEY", "sk-first")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if updated.IsZero() || time.Since(updated) > time.Minute {
		t.Errorf("Set answered %v, want the moment it was written", updated)
	}

	held, err := s.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(held) != 1 || held[0].Name != "ANTHROPIC_API_KEY" {
		t.Fatalf("List answered %+v, want the one name", held)
	}

	value, found, err := s.Get("alice", "ANTHROPIC_API_KEY")
	if err != nil || !found || value != "sk-first" {
		t.Fatalf("Get answered %q (found %t, err %v), want the value that was set", value, found, err)
	}

	// Setting again is the whole of a rotation: the value is replaced and
	// nothing else happens, see PLAN.md section 2.3.
	if _, err := s.Set("alice", "ANTHROPIC_API_KEY", "sk-second"); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	value, _, err = s.Get("alice", "ANTHROPIC_API_KEY")
	if err != nil || value != "sk-second" {
		t.Fatalf("Get answered %q (err %v), want the value the rotation wrote", value, err)
	}

	removed, err := s.Remove("alice", "ANTHROPIC_API_KEY")
	if err != nil || !removed {
		t.Fatalf("Remove answered %t (err %v), want it removed", removed, err)
	}
	// Removing it again is not a failure: the call describes the state it
	// wants, which is the invariant in PLAN.md section 2.6.
	removed, err = s.Remove("alice", "ANTHROPIC_API_KEY")
	if err != nil || removed {
		t.Fatalf("the second Remove answered %t (err %v), want false and no failure", removed, err)
	}
	if _, found, _ := s.Get("alice", "ANTHROPIC_API_KEY"); found {
		t.Error("the value is still readable after it was removed")
	}
}

// A name that is not an environment variable name is refused, which is the
// guard that keeps a path out of a name: without it "../../etc/passwd" is a
// file this package would write.
func TestSetRefusesANameThatIsNotOne(t *testing.T) {
	s, _ := store(t)
	for _, name := range []string{
		"", "anthropic_api_key", "1KEY", "A-B", "KEY.NAME", "../SECRET", "KEY/SUB",
		strings.Repeat("A", 65), "KEY ", " KEY",
	} {
		if _, err := s.Set("alice", name, "value"); !errors.Is(err, secrets.ErrName) {
			t.Errorf("Set(%q) = %v, want ErrName", name, err)
		}
		if _, _, err := s.Get("alice", name); !errors.Is(err, secrets.ErrName) {
			t.Errorf("Get(%q) = %v, want ErrName", name, err)
		}
		if _, err := s.Remove("alice", name); !errors.Is(err, secrets.ErrName) {
			t.Errorf("Remove(%q) = %v, want ErrName", name, err)
		}
	}
	// The longest name a member may hold is still a name.
	if _, err := s.Set("alice", "A"+strings.Repeat("B", 63), "value"); err != nil {
		t.Errorf("a 64 character name was refused: %v", err)
	}
}

// The member is a path component too, and it is held to the same pattern
// users_create is: without this guard a member name is a way out of the tree.
func TestRefusesAMemberNameKitbashDoesNotCreate(t *testing.T) {
	s, _ := store(t)
	for _, member := range []string{"", "..", "../root", "Alice", "a/b", "alice/../bob"} {
		if _, err := s.Set(member, "KEY", "value"); !errors.Is(err, secrets.ErrMember) {
			t.Errorf("Set for %q = %v, want ErrMember", member, err)
		}
		if _, err := s.List(member); !errors.Is(err, secrets.ErrMember) {
			t.Errorf("List for %q = %v, want ErrMember", member, err)
		}
		if err := s.RemoveMember(member); !errors.Is(err, secrets.ErrMember) {
			t.Errorf("RemoveMember for %q = %v, want ErrMember", member, err)
		}
	}
}

// A value is one line of an environment file, so the four shapes that cannot
// be one are refused. Each is its own guard: an 8 KiB limit that let a line
// break through would write a file podman reads as two variables.
func TestSetRefusesAValueNoEnvironmentFileCarries(t *testing.T) {
	s, _ := store(t)
	cases := map[string]string{
		"empty":             "",
		"over the size":     strings.Repeat("x", secrets.MaxValueBytes+1),
		"a NUL byte":        "sk-\x00-key",
		"a line break":      "sk-first\nsk-second",
		"a carriage return": "sk-first\rsk-second",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Set("alice", "KEY", value); !errors.Is(err, secrets.ErrValue) {
				t.Errorf("Set = %v, want ErrValue", err)
			}
			if _, found, _ := s.Get("alice", "KEY"); found {
				t.Error("a refused value was written")
			}
		})
	}
	// The longest value a member may set is still a value.
	if _, err := s.Set("alice", "KEY", strings.Repeat("x", secrets.MaxValueBytes)); err != nil {
		t.Errorf("a value of exactly %d bytes was refused: %v", secrets.MaxValueBytes, err)
	}
}

// No refusal quotes the value, which is what keeps a secret out of a problem
// detail and out of every log line the daemon writes about one.
func TestAValueRefusalDoesNotQuoteTheValue(t *testing.T) {
	s, _ := store(t)
	const value = "sk-do-not-repeat-me\nsecond line"
	_, err := s.Set("alice", "KEY", value)
	if err == nil {
		t.Fatal("a value with a line break was accepted")
	}
	if strings.Contains(err.Error(), "sk-do-not-repeat-me") {
		t.Errorf("the refusal is %q, want one that does not quote the value", err)
	}
}

// The tree is root owned and narrow: 0700 on every directory and 0600 on every
// value. Without it a member on the host reads another member's credentials
// with cat.
func TestTheTreeIsNarrow(t *testing.T) {
	s, dir := store(t)
	if _, err := s.Set("alice", "KEY", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "alice")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != secrets.DirMode {
			t.Errorf("%s is %v, want %v", path, info.Mode().Perm(), os.FileMode(secrets.DirMode))
		}
	}
	info, err := os.Stat(filepath.Join(dir, "alice", "KEY"))
	if err != nil {
		t.Fatalf("stat the value: %v", err)
	}
	if info.Mode().Perm() != secrets.FileMode {
		t.Errorf("the value is %v, want %v", info.Mode().Perm(), os.FileMode(secrets.FileMode))
	}
}

// A write is a temporary file renamed onto the name, and the temporary file is
// gone when the call returns: a listing that showed one would report a name no
// unit can declare, and a leftover would be a second copy of a credential.
func TestSetLeavesNoTemporaryFile(t *testing.T) {
	s, dir := store(t)
	if _, err := s.Set("alice", "KEY", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "alice"))
	if err != nil {
		t.Fatalf("reading the member's directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "KEY" {
		t.Errorf("the directory holds %v, want the one value", names)
	}
}

// One member reads none of another's. There is no call here that takes two
// member names, and this is the property that makes that matter.
func TestOneMemberReadsNoneOfAnother(t *testing.T) {
	s, _ := store(t)
	if _, err := s.Set("alice", "KEY", "alice-value"); err != nil {
		t.Fatalf("Set for alice: %v", err)
	}
	if _, err := s.Set("bob", "KEY", "bob-value"); err != nil {
		t.Fatalf("Set for bob: %v", err)
	}

	value, _, err := s.Get("bob", "KEY")
	if err != nil || value != "bob-value" {
		t.Fatalf("bob reads %q (err %v), want their own value", value, err)
	}
	held, err := s.List("bob")
	if err != nil || len(held) != 1 {
		t.Fatalf("bob's list is %+v (err %v), want their one name", held, err)
	}

	// Removing one member's name leaves the other's alone, which is what makes
	// the directory per member rather than the name unique on the host.
	if _, err := s.Remove("bob", "KEY"); err != nil {
		t.Fatalf("Remove for bob: %v", err)
	}
	value, found, err := s.Get("alice", "KEY")
	if err != nil || !found || value != "alice-value" {
		t.Errorf("alice reads %q (found %t, err %v) after bob removed theirs, want their own value",
			value, found, err)
	}
}

// A member who has never set one lists nothing rather than failing, and a
// member removed with their account keeps nothing behind.
func TestRemoveMemberTakesTheWholeSet(t *testing.T) {
	s, dir := store(t)
	if _, err := s.Set("alice", "KEY", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.RemoveMember("alice"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alice")); !os.IsNotExist(err) {
		t.Errorf("the directory of a removed member is still there: %v", err)
	}
	held, err := s.List("alice")
	if err != nil || len(held) != 0 {
		t.Errorf("List answered %+v (err %v), want an empty list", held, err)
	}
	if _, found, err := s.Get("alice", "KEY"); found || err != nil {
		t.Errorf("Get answered found %t (err %v) for a member whose set is gone", found, err)
	}
}

// A symlink anywhere below the base directory is refused rather than followed.
// The tree is root owned, so only root can plant one, and a link followed here
// would write a member's credential into a file of somebody else's choosing.
func TestASymlinkIsRefused(t *testing.T) {
	s, dir := store(t)
	elsewhere := t.TempDir()

	// A member's whole directory replaced by a link out of the tree.
	if err := os.Symlink(elsewhere, filepath.Join(dir, "alice")); err != nil {
		t.Fatalf("planting the directory link: %v", err)
	}
	if _, err := s.Set("alice", "KEY", "value"); err == nil {
		t.Error("a set through a linked member directory was allowed")
	}
	if _, _, err := s.Get("alice", "KEY"); err == nil {
		t.Error("a get through a linked member directory was allowed")
	}
	if _, err := s.List("alice"); err == nil {
		t.Error("a list through a linked member directory was allowed")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("the link target holds %d entries, want nothing written through the link", len(entries))
	}

	// One name replaced by a link to a file outside the tree.
	if err := os.Remove(filepath.Join(dir, "alice")); err != nil {
		t.Fatalf("removing the directory link: %v", err)
	}
	if _, err := s.Set("bob", "OTHER", "value"); err != nil {
		t.Fatalf("Set for bob: %v", err)
	}
	target := filepath.Join(elsewhere, "target")
	if err := os.WriteFile(target, []byte("theirs"), 0o600); err != nil {
		t.Fatalf("writing the link target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "bob", "KEY")); err != nil {
		t.Fatalf("planting the value link: %v", err)
	}
	if _, _, err := s.Get("bob", "KEY"); err == nil {
		t.Error("a get of a linked value was allowed")
	}
	// A set replaces the link with a file of this tree rather than writing
	// through it: rename acts on the name, not on what it points at.
	if _, err := s.Set("bob", "KEY", "ours"); err != nil {
		t.Fatalf("Set over the link: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "theirs" {
		t.Errorf("the link target holds %q (err %v), want the file it had before the set", body, err)
	}
	value, _, err := s.Get("bob", "KEY")
	if err != nil || value != "ours" {
		t.Errorf("Get answered %q (err %v) after the set, want the value that was set", value, err)
	}
}

// A listing carries names and timestamps and no value anywhere in it. The
// shape is the guard: a field added to Entry would show up here.
func TestAListingCarriesNoValue(t *testing.T) {
	s, _ := store(t)
	if _, err := s.Set("alice", "KEY", "sk-do-not-list-me"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	held, err := s.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	body, err := json.Marshal(held)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "sk-do-not-list-me") {
		t.Errorf("the listing is %s, want no value in it", body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, row := range rows {
		for key := range row {
			if key != "name" && key != "updated" {
				t.Errorf("a listed secret carries %q, want name and updated only", key)
			}
		}
	}
}

// A file that is not a value is not listed and not read: a directory, a
// socket, or a temporary file a killed daemon left behind.
func TestOnlyValuesAreListed(t *testing.T) {
	s, dir := store(t)
	if _, err := s.Set("alice", "KEY", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alice", "KEY.123.tmp"), []byte("half"), 0o600); err != nil {
		t.Fatalf("planting a temporary file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "alice", "NESTED"), 0o700); err != nil {
		t.Fatalf("planting a directory: %v", err)
	}
	held, err := s.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(held) != 1 || held[0].Name != "KEY" {
		t.Errorf("List answered %+v, want the one value", held)
	}
	if _, found, err := s.Get("alice", "NESTED"); found || err == nil {
		t.Errorf("Get of a directory answered found %t, err %v, want a refusal", found, err)
	}
}
