package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The M8 half of the job: what a Process can see of Files, see PLAN.md section
// 2.3. The member writes a file with fs_write, a Package whose unit mounts that
// folder read only reads it back through the kernel, a second Package mounted
// read write writes a file the member then reads with fs_read, and the three
// mounts a member may not have are refused at proc_run.
//
// Every step here is the member's, not the admin's: the rules are about a
// member's own home and about /org being read only for everyone, and a job that
// proved them as an administrator would have proved the easier half.

// The two Packages of this half and the two folders of the member's home they
// mount. The Packages are written from one fixture with its manifest filled in,
// so what differs between them is the mount and nothing else.
const (
	readerName = "reader"
	writerName = "writer"
	notesName  = "notes"
	outName    = "out"

	readerRead  = "reader_read"
	readerWrite = "reader_write"
	writerWrite = "writer_write"

	// Where each Package sees its folder, and the files on either side of the
	// mount: one the member wrote for the Process to read, one the Process
	// writes for the member to read.
	notesMount = "/files/notes"
	outMount   = "/files/out"
	noteFile   = "today.md"
	madeFile   = "made.md"
)

// madeText is what the read write Process writes and the member reads back with
// fs_read. It is the other direction of the same mount.
const madeText = "Written by a Process through a read write mount.\n"

// folderManifest is what makes a folder of Files visible, and therefore
// mountable: a folder without one does not exist as far as the surface is
// concerned, and a Process cannot be given it either.
func folderManifest(name, description string) string {
	return "name: " + name + "\ndescription: >-\n  " + description + "\n"
}

// writeMountFixture writes one Package of the reader fixture with its manifest
// filled in: the name, the folder it mounts, where the container sees it and
// whether it may write there. The fixture carries a template rather than a
// finished manifest because the member's name is not known until the job runs.
func writeMountFixture(t *testing.T, session *session, target, name, source, mount, mode string) {
	t.Helper()
	for _, file := range fixtures {
		body, err := os.ReadFile(filepath.Join("fixtures", readerName, file))
		if err != nil {
			t.Fatalf("reading the reader fixture %s: %v", file, err)
		}
		content := string(body)
		if file == "kitbash.yaml" {
			content = strings.NewReplacer(
				"__NAME__", name,
				"__SOURCE__", source,
				"__TARGET__", mount,
				"__MODE__", mode,
			).Replace(content)
		}
		writeFile(t, session, filepath.Join(target, file), content)
	}
}

// rewriteReaderMount writes the reader Package's manifest again with one mount
// changed, which is how the refusals below are driven: the image is already
// built, so proc_run reaches the mounts rather than stopping at a Package that
// has never been built.
func rewriteReaderMount(t *testing.T, s *state, source, mount, mode string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("fixtures", readerName, "kitbash.yaml"))
	if err != nil {
		t.Fatalf("reading the reader manifest: %v", err)
	}
	content := strings.NewReplacer(
		"__NAME__", readerName,
		"__SOURCE__", source,
		"__TARGET__", mount,
		"__MODE__", mode,
	).Replace(string(body))
	writeFile(t, s.member, filepath.Join(s.readerPath, "kitbash.yaml"), content)
}

// memberWritesTheFiles is the first half of the acceptance sentence: a member
// writes a file with fs_write, in a folder that carries a manifest and is
// therefore visible. The second folder is the one the read write Package writes
// into, and it is made visible the same way, because an invisible folder is
// unmountable whichever way it would be mounted.
func memberWritesTheFiles(t *testing.T, s *state) {
	// The member's session was closed when the admin approved their write, so
	// this half opens its own.
	s.member = dial(t, memberName())

	writeFile(t, s.member, filepath.Join(s.notesPath, "kitbash.yaml"),
		folderManifest(notesName, "Notes a Process reads through a read only mount."))
	writeFile(t, s.member, filepath.Join(s.notesPath, noteFile), s.noteText)
	writeFile(t, s.member, filepath.Join(s.outPath, "kitbash.yaml"),
		folderManifest(outName, "Where a Process writes through a read write mount."))
}

// runTheReader builds and runs the Package whose unit mounts the notes folder
// read only. Its tools join the member's surface like any other Package with
// expose: mcp.
func runTheReader(t *testing.T, s *state) {
	writeMountFixture(t, s.member, s.readerPath, readerName, s.notesPath, notesMount, "ro")

	s.member.callWithin(buildTimeout, "pkg_build",
		map[string]any{"path": s.readerPath}).mustSucceed(t, "pkg_build")

	var out struct {
		ID     string   `json:"id"`
		State  string   `json:"state"`
		Tools  []string `json:"tools"`
		Mounts []struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Mode   string `json:"mode"`
		} `json:"mounts"`
	}
	s.member.ok("proc_run", map[string]any{"package": s.readerPath}, &out)
	if out.State != "running" {
		t.Fatalf("proc_run answered %+v, want a running Process", out)
	}
	// proc_run reports the mounts as kitbashd resolved them, which is how a
	// member reads what their Process can reach.
	if len(out.Mounts) != 1 || out.Mounts[0].Source != s.notesPath ||
		out.Mounts[0].Target != notesMount || out.Mounts[0].Mode != "ro" {
		t.Fatalf("proc_run reported the mounts %+v, want the notes folder read only at %s",
			out.Mounts, notesMount)
	}
	if !hasTool(out.Tools, readerRead) || !hasTool(out.Tools, readerWrite) {
		t.Fatalf("proc_run published %v, want %s and %s", out.Tools, readerRead, readerWrite)
	}
}

// readThroughTheMount is the acceptance sentence itself: the tool of a Process
// answers with the file its owner wrote through fs_write, having reached it
// through the kernel and nothing else. The Package permits no tool over /mcp at
// all, so this cannot have gone through the surface.
func readThroughTheMount(t *testing.T, s *state) {
	var out struct {
		Content string `json:"content"`
	}
	s.member.ok(readerRead, map[string]any{"path": notesMount + "/" + noteFile}, &out)
	if out.Content != s.noteText {
		t.Fatalf("%s answered %q, want the note the member wrote, %q", readerRead, out.Content, s.noteText)
	}
}

// writeIntoTheReadOnlyMount is the mode being a kernel fact: the container is
// the member and could write this folder through any other path, and the mount
// is what stops it. The refusal is the tool's own error rather than a problem,
// because nothing in kitbash refused it.
func writeIntoTheReadOnlyMount(t *testing.T, s *state) {
	res := s.member.call(readerWrite, map[string]any{
		"path":    notesMount + "/" + madeFile,
		"content": "this must not be written",
	})
	if !res.IsError {
		t.Fatalf("%s wrote into a read only mount: %s", readerWrite, truncate(res.text()))
	}
	if !strings.Contains(strings.ToLower(res.text()), "read-only") {
		t.Fatalf("%s failed with %q, want the kernel's read only refusal", readerWrite, truncate(res.text()))
	}
	// Nothing was written, which the member reads with fs_read.
	refused := s.member.call("fs_read", map[string]any{"path": filepath.Join(s.notesPath, madeFile)})
	refused.mustProblem(t, "fs_read", "not-found")
}

// runTheWriter is the other half of the acceptance sentence: a second Package,
// mounted read write, writes a file into a folder of the member's home.
func runTheWriter(t *testing.T, s *state) {
	writeMountFixture(t, s.member, s.writerPath, writerName, s.outPath, outMount, "rw")

	s.member.callWithin(buildTimeout, "pkg_build",
		map[string]any{"path": s.writerPath}).mustSucceed(t, "pkg_build")

	var out struct {
		State  string   `json:"state"`
		Tools  []string `json:"tools"`
		Mounts []struct {
			Mode string `json:"mode"`
		} `json:"mounts"`
	}
	s.member.ok("proc_run", map[string]any{"package": s.writerPath}, &out)
	if out.State != "running" || len(out.Mounts) != 1 || out.Mounts[0].Mode != "rw" {
		t.Fatalf("proc_run answered %+v, want a running Process with one read write mount", out)
	}

	var written struct {
		Written bool `json:"written"`
	}
	s.member.ok(writerWrite, map[string]any{
		"path": outMount + "/" + madeFile, "content": madeText,
	}, &written)
	if !written.Written {
		t.Fatalf("%s answered %+v, want the file written", writerWrite, written)
	}
}

// readWhatTheProcessWrote closes the loop the other way: the member reads the
// file their Process wrote, with fs_read, as an ordinary file of Files.
//
// It has no commit, and that is the known hole PLAN.md 2.1 records: a container
// writing through a mount writes into the working tree, so fs_read sees the
// file at once and fs_history does not. The job asserts the hole rather than
// leaving it to be discovered.
func readWhatTheProcessWrote(t *testing.T, s *state) {
	// fs_read declares no output schema: the file is the first content block
	// and the metadata is the last, see spec/mcp-surface.yaml.
	read := s.member.call("fs_read", map[string]any{"path": filepath.Join(s.outPath, madeFile)})
	read.mustSucceed(t, "fs_read")
	if len(read.Content) == 0 || read.Content[0].Text != madeText {
		t.Fatalf("fs_read answered %q, want what the Process wrote, %q",
			truncate(read.text()), madeText)
	}
	var history struct {
		Commits []struct {
			Message string `json:"message"`
		} `json:"commits"`
	}
	s.member.ok("fs_history", map[string]any{"path": filepath.Join(s.outPath, madeFile)}, &history)
	if len(history.Commits) != 0 {
		t.Fatalf("fs_history holds %+v for a file a Process wrote, want none: a mount makes no commit",
			history.Commits)
	}
}

// illegalMounts is the last sentence of M8: the three mounts a member may not
// have, refused at proc_run, each with the problem that says which rule it
// broke. The Package is already built, so what these reach is the mount check
// and not a Package that was never built.
func illegalMounts(t *testing.T, s *state) {
	// A link out of the member's home, planted inside a visible folder, which
	// is the escape the resolution exists to refuse. fs_write writes no links,
	// so it is made on the host as the member, which is what a member with a
	// shell can do.
	link := filepath.Join(s.notesPath, "away")
	if out, err := runAs(t, memberName(), "ln", "-sfn", "/etc", link); err != nil {
		t.Fatalf("planting the escaping link: %v\n%s", err, out)
	}

	for _, c := range []struct {
		name   string
		source string
		mode   string
		slug   string
	}{
		{"another member's home", filepath.Join("/home", adminName(), packageName), "ro", "not-permitted"},
		{"/org read write", "/org/handbook", "rw", "not-permitted"},
		{"a link out of the home", link, "ro", "invalid-path"},
	} {
		t.Logf("illegal mount: %s", c.name)
		rewriteReaderMount(t, s, c.source, notesMount, c.mode)
		res := s.member.call("proc_run", map[string]any{
			"package": s.readerPath, "name": "illegal",
		})
		res.mustProblem(t, "proc_run", c.slug)
	}

	// The Process that was already running is untouched: a refused run removes
	// nothing, so the member still has the reader they started.
	var out struct {
		Content string `json:"content"`
	}
	s.member.ok(readerRead, map[string]any{"path": notesMount + "/" + noteFile}, &out)
	if out.Content != s.noteText {
		t.Fatalf("%s answered %q after three refused runs, want the note still readable", readerRead, out.Content)
	}
}

// hasTool reports whether a tool list carries one name.
func hasTool(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
