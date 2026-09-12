package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/server"
)

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// fixture builds a tree that exercises every visibility rule and returns the
// two roots.
type fixture struct {
	org     string
	home    string
	outside string
	files   *fs.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	org := filepath.Join(base, "org")
	home := filepath.Join(base, "home", "tester")
	mkdir(t, filepath.Join(org, "handbook", "policies"))
	mkdir(t, filepath.Join(org, "handbook", "drafts"))
	mkdir(t, filepath.Join(org, "handbook", "broken"))
	mkdir(t, filepath.Join(org, "nomanifest"))
	mkdir(t, filepath.Join(org, "invalid"))
	mkdir(t, filepath.Join(home, "notes"))

	// A visible top level folder.
	write(t, filepath.Join(org, "handbook", "kitbash.yaml"), `name: handbook
description: How this organization works. Read before writing anything into /org.
tags: [docs, onboarding]
`)
	write(t, filepath.Join(org, "handbook", "README.md"), "# Handbook\n\nStart here.\n")
	write(t, filepath.Join(org, "handbook", ".secret"), "not part of the surface\n")
	write(t, filepath.Join(org, "handbook", "logo.png"), string(tinyPNG(t)))
	write(t, filepath.Join(org, "handbook", "archive.bin"), "\x00\x01\x02\x03binary payload")

	// A visible subfolder, an invisible one, and one with a broken manifest.
	write(t, filepath.Join(org, "handbook", "policies", "kitbash.yaml"), `name: policies
description: The rules everyone follows, one file per policy area.
`)
	write(t, filepath.Join(org, "handbook", "drafts", "draft.md"), "unfinished\n")
	write(t, filepath.Join(org, "handbook", "broken", "kitbash.yaml"), `name: Broken Name
description: short
colour: blue
`)

	// A top level folder with no manifest at all, holding a folder that has a
	// perfectly good one. The inner folder is still outside the surface.
	write(t, filepath.Join(org, "nomanifest", "notes.md"), "invisible\n")
	mkdir(t, filepath.Join(org, "nomanifest", "inner"))
	write(t, filepath.Join(org, "nomanifest", "inner", "kitbash.yaml"), `name: inner
description: A folder with a valid manifest under a folder that has none.
`)
	write(t, filepath.Join(org, "nomanifest", "inner", "buried.md"), "buried\n")
	// A top level folder whose manifest does not validate.
	write(t, filepath.Join(org, "invalid", "kitbash.yaml"), "name: invalid\n")

	// The caller's home.
	write(t, filepath.Join(home, "notes", "kitbash.yaml"), `name: notes
description: Personal notes that belong to this user and nobody else.
`)
	write(t, filepath.Join(home, "notes", "todo.md"), "- ship M1\n")

	gitInit(t, filepath.Join(org, "handbook"))
	gitInit(t, filepath.Join(home, "notes"))

	// Outside every root, so the symlink repros have somewhere to point.
	outside := filepath.Join(base, "outside")
	mkdir(t, outside)
	write(t, filepath.Join(outside, "secret.txt"), "the crown jewels\n")

	t.Setenv(fs.SSHEnv, "")
	t.Setenv(fs.RootsEnv, org+":"+home)
	files, err := fs.NewFromEnv()
	if err != nil {
		t.Fatalf("fs.NewFromEnv: %v", err)
	}
	return &fixture{org: org, home: home, outside: outside, files: files}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 1, color.RGBA{B: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding png: %v", err)
	}
	return buf.Bytes()
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@kitbash",
			"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@kitbash")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	run("add", "-A")
	run("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "Seed the fixture")
}

// connect drives the real MCP path: initialize over an in memory transport.
func connect(t *testing.T, f *fixture) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	srv := server.New("test", server.Deps{Files: f.files})
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kitbash-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		serverSession.Wait()
	})
	return session
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func ok(t *testing.T, res *mcp.CallToolResult, name string) {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s failed: %s", name, textOf(t, res))
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, isText := res.Content[0].(*mcp.TextContent)
	if !isText {
		t.Fatalf("first content is %T, want text", res.Content[0])
	}
	return tc.Text
}

// problemOf asserts that a call failed with one text content holding RFC 9457
// problem details.
func problemOf(t *testing.T, res *mcp.CallToolResult) problem.Problem {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected an error result, got %s", textOf(t, res))
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected exactly one content block, got %d", len(res.Content))
	}
	var p problem.Problem
	if err := json.Unmarshal([]byte(textOf(t, res)), &p); err != nil {
		t.Fatalf("error content is not problem details: %v", err)
	}
	if p.Title == "" || p.Status == 0 || p.Detail == "" {
		t.Fatalf("problem is missing required fields: %+v", p)
	}
	return p
}

// readMeta decodes the trailing text block of fs_read, which is where the
// metadata lives now that the tool returns no structured content.
func readMeta(t *testing.T, res *mcp.CallToolResult) fs.ReadMeta {
	t.Helper()
	if res.StructuredContent != nil {
		t.Fatalf("fs_read must not return structured content, got %v", res.StructuredContent)
	}
	if len(res.Content) != 2 {
		t.Fatalf("expected the file and its metadata, got %d content blocks", len(res.Content))
	}
	tc, isText := res.Content[1].(*mcp.TextContent)
	if !isText {
		t.Fatalf("the metadata block is %T, want text", res.Content[1])
	}
	var meta fs.ReadMeta
	if err := json.Unmarshal([]byte(tc.Text), &meta); err != nil {
		t.Fatalf("decoding metadata %s: %v", tc.Text, err)
	}
	return meta
}

func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshaling structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding structured content %s: %v", raw, err)
	}
	return out
}

func TestInitializeAndToolsList(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	if got := s.InitializeResult().ServerInfo.Name; got != "kitbash" {
		t.Errorf("server name is %q, want kitbash", got)
	}
	if got := s.InitializeResult().ServerInfo.Version; got != "test" {
		t.Errorf("server version is %q, want test", got)
	}

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
		// fs_read is content blocks only, see spec/mcp-surface.yaml.
		if tool.Name == "fs_read" {
			if tool.OutputSchema != nil {
				t.Error("fs_read must not declare an output schema")
			}
		} else if tool.OutputSchema == nil {
			t.Errorf("tool %s has no output schema", tool.Name)
		}
	}
	want := []string{"fs_history", "fs_list", "fs_read", "fs_write"}
	if len(names) != len(want) {
		t.Fatalf("tools/list returned %v, want exactly %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Errorf("tool %d is %s, want %s", i, names[i], name)
		}
	}
}

func TestListRootsShowsOnlyVisibleFolders(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	res := call(t, s, "fs_list", map[string]any{})
	ok(t, res, "fs_list")
	out := structured[fs.ListResult](t, res)

	if out.Path != "/" {
		t.Errorf("roots listing path is %q, want /", out.Path)
	}
	if len(out.Files) != 0 {
		t.Errorf("roots listing returned files: %+v", out.Files)
	}
	byName := map[string]fs.FolderEntry{}
	for _, folder := range out.Folders {
		byName[folder.Name] = folder
	}
	if _, found := byName["handbook"]; !found {
		t.Errorf("handbook is missing from %+v", out.Folders)
	}
	if _, found := byName["notes"]; !found {
		t.Errorf("the caller's home folder notes is missing from %+v", out.Folders)
	}
	if len(out.Folders) != 2 {
		t.Errorf("roots listing returned %d folders, want 2: %+v", len(out.Folders), out.Folders)
	}
	if got := byName["handbook"].Description; got == "" {
		t.Error("handbook has no description")
	}
	if got := byName["handbook"].Tags; len(got) != 2 {
		t.Errorf("handbook tags are %v, want two", got)
	}
	if byName["handbook"].Package {
		t.Error("handbook has no deploy block, so it is not a package")
	}
}

func TestListFolderHidesDotfilesAndInvisibleFolders(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	res := call(t, s, "fs_list", map[string]any{"path": filepath.Join(f.org, "handbook")})
	ok(t, res, "fs_list")
	out := structured[fs.ListResult](t, res)

	if out.Manifest["name"] != "handbook" {
		t.Errorf("manifest is %v, want the handbook manifest", out.Manifest)
	}
	if len(out.Folders) != 1 || out.Folders[0].Name != "policies" {
		t.Errorf("subfolders are %+v, want policies only", out.Folders)
	}
	names := map[string]fs.FileEntry{}
	for _, file := range out.Files {
		names[file.Name] = file
	}
	for _, want := range []string{"README.md", "kitbash.yaml", "logo.png"} {
		if _, found := names[want]; !found {
			t.Errorf("%s is missing from %+v", want, out.Files)
		}
	}
	if _, found := names[".secret"]; found {
		t.Error("a dotfile appeared in the listing")
	}
	if got := names["README.md"].MediaType; got != "text/markdown" {
		t.Errorf("README.md media type is %q, want text/markdown", got)
	}
	if got := names["logo.png"].MediaType; got != "image/png" {
		t.Errorf("logo.png media type is %q, want image/png", got)
	}
	if names["README.md"].Size == 0 || names["README.md"].Modified == "" {
		t.Errorf("README.md entry is incomplete: %+v", names["README.md"])
	}
}

func TestListNotVisible(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	for _, dir := range []string{"nomanifest", "invalid"} {
		p := problemOf(t, call(t, s, "fs_list", map[string]any{"path": filepath.Join(f.org, dir)}))
		if p.Slug() != problem.SlugNotVisible {
			t.Errorf("%s returned %s, want not-visible", dir, p.Slug())
		}
	}
}

func TestReadTextReturnsTextAndMetadata(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	res := call(t, s, "fs_read", map[string]any{"path": filepath.Join(f.org, "handbook", "README.md")})
	ok(t, res, "fs_read")
	if len(res.Content) != 2 {
		t.Fatalf("expected content and metadata blocks, got %d", len(res.Content))
	}
	if got := textOf(t, res); got != "# Handbook\n\nStart here.\n" {
		t.Errorf("content is %q", got)
	}
	meta := readMeta(t, res)
	if meta.MediaType != "text/markdown" {
		t.Errorf("media type is %q, want text/markdown", meta.MediaType)
	}
	if meta.Size == 0 {
		t.Error("size is zero")
	}
	if !shaPattern.MatchString(meta.Sha) {
		t.Errorf("sha is %q, want the commit that last touched the file", meta.Sha)
	}
	if meta.Truncated {
		t.Error("a whole file read is not truncated")
	}
}

func TestReadTextWindow(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	res := call(t, s, "fs_read", map[string]any{
		"path":   filepath.Join(f.org, "handbook", "README.md"),
		"offset": 2,
		"limit":  8,
	})
	ok(t, res, "fs_read")
	if got := textOf(t, res); got != "Handbook" {
		t.Errorf("window is %q, want Handbook", got)
	}
	if meta := readMeta(t, res); !meta.Truncated {
		t.Error("a window read is truncated")
	}
}

func TestReadImageReturnsImageContent(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	path := filepath.Join(f.org, "handbook", "logo.png")
	res := call(t, s, "fs_read", map[string]any{"path": path})
	ok(t, res, "fs_read")
	img, isImage := res.Content[0].(*mcp.ImageContent)
	if !isImage {
		t.Fatalf("first content is %T, want image", res.Content[0])
	}
	if img.MIMEType != "image/png" {
		t.Errorf("mime type is %q, want image/png", img.MIMEType)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture image: %v", err)
	}
	if !bytes.Equal(img.Data, onDisk) {
		t.Error("image data does not round trip")
	}
	if meta := readMeta(t, res); meta.MediaType != "image/png" {
		t.Errorf("metadata media type is %q, want image/png", meta.MediaType)
	}
}

func TestWriteCommitsAsTheCaller(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	path := filepath.Join(f.org, "handbook", "policies", "security.md")
	res := call(t, s, "fs_write", map[string]any{
		"path":    path,
		"content": "# Security\n\nRotate keys yearly.\n",
		"message": "Add the security policy",
	})
	ok(t, res, "fs_write")
	out := structured[fs.WriteResult](t, res)
	if !shaPattern.MatchString(out.Commit.Sha) {
		t.Errorf("commit sha is %q", out.Commit.Sha)
	}
	if out.Commit.Author != f.files.User() {
		t.Errorf("commit author is %q, want %q", out.Commit.Author, f.files.User())
	}
	if out.Commit.Message != "Add the security policy" {
		t.Errorf("commit message is %q", out.Commit.Message)
	}
	if out.Commit.Time == "" {
		t.Error("commit time is empty")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if string(onDisk) != "# Security\n\nRotate keys yearly.\n" {
		t.Errorf("file content is %q", onDisk)
	}

	history := call(t, s, "fs_history", map[string]any{"path": path})
	ok(t, history, "fs_history")
	commits := structured[fs.HistoryResult](t, history)
	if len(commits.Commits) != 1 || commits.Commits[0].Sha != out.Commit.Sha {
		t.Errorf("history is %+v, want the commit just made", commits.Commits)
	}
}

func TestWriteExpectedShaConflict(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	path := filepath.Join(f.org, "handbook", "README.md")
	res := call(t, s, "fs_write", map[string]any{
		"path":        path,
		"content":     "# Handbook\n\nRewritten.\n",
		"message":     "Rewrite the handbook",
		"expectedSha": "0000000000000000000000000000000000000000",
	})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugConflict {
		t.Errorf("slug is %s, want conflict", p.Slug())
	}
	if p.Status != 409 {
		t.Errorf("status is %d, want 409", p.Status)
	}
	if p.Fix == "" {
		t.Error("a conflict must carry a fix")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(onDisk, []byte("Start here")) {
		t.Errorf("the refused write touched the file: %q, %v", onDisk, err)
	}
}

func TestWriteManifestMakesAFolderVisible(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	path := filepath.Join(f.home, "recipes", "kitbash.yaml")
	bad := call(t, s, "fs_write", map[string]any{
		"path":    path,
		"content": "name: Recipes\ndescription: too short\n",
		"message": "Add a manifest",
	})
	p := problemOf(t, bad)
	if p.Slug() != problem.SlugInvalidManifest {
		t.Fatalf("slug is %s, want invalid-manifest", p.Slug())
	}
	if p.Status != 422 {
		t.Errorf("status is %d, want 422", p.Status)
	}
	if p.Detail == "" {
		t.Error("the validator messages are missing from detail")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an invalid manifest was written to disk")
	}

	good := call(t, s, "fs_write", map[string]any{
		"path":    path,
		"content": "name: recipes\ndescription: Things this user cooks, one file per recipe.\n",
		"message": "Add the recipes manifest",
	})
	ok(t, good, "fs_write")

	res := call(t, s, "fs_list", map[string]any{})
	ok(t, res, "fs_list")
	out := structured[fs.ListResult](t, res)
	found := false
	for _, folder := range out.Folders {
		if folder.Name == "recipes" {
			found = true
		}
	}
	if !found {
		t.Errorf("recipes did not become visible: %+v", out.Folders)
	}
}

func TestInvalidPath(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	cases := map[string]map[string]any{
		"parent reference":  {"path": f.org + "/handbook/../invalid"},
		"outside the roots": {"path": "/etc"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			p := problemOf(t, call(t, s, "fs_list", args))
			if p.Slug() != problem.SlugInvalidPath {
				t.Errorf("slug is %s, want invalid-path", p.Slug())
			}
			if p.Status != 400 {
				t.Errorf("status is %d, want 400", p.Status)
			}
		})
	}
}

func TestReadUnsupportedMediaType(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	p := problemOf(t, call(t, s, "fs_read", map[string]any{
		"path": filepath.Join(f.org, "handbook", "archive.bin"),
	}))
	if p.Slug() != problem.SlugUnsupported {
		t.Errorf("slug is %s, want unsupported-media-type", p.Slug())
	}
	if p.Status != 415 {
		t.Errorf("status is %d, want 415", p.Status)
	}
}

func TestWriteBase64(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	path := filepath.Join(f.org, "handbook", "policies", "seal.png")
	image := tinyPNG(t)
	ok(t, call(t, s, "fs_write", map[string]any{
		"path":          path,
		"contentBase64": base64.StdEncoding.EncodeToString(image),
		"message":       "Add the policy seal",
	}), "fs_write")
	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(onDisk, image) {
		t.Fatalf("base64 content did not round trip: %v", err)
	}

	p := problemOf(t, call(t, s, "fs_write", map[string]any{
		"path":          path,
		"contentBase64": "this is not base64!!",
		"message":       "Break the seal",
	}))
	if p.Slug() != problem.SlugBadRequest {
		t.Errorf("slug is %s, want bad-request", p.Slug())
	}
	if p.Status != 400 {
		t.Errorf("status is %d, want 400", p.Status)
	}
}

func TestListRootPathIsTheRootsView(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	res := call(t, s, "fs_list", map[string]any{"path": f.org})
	ok(t, res, "fs_list")
	out := structured[fs.ListResult](t, res)
	if out.Manifest != nil {
		t.Errorf("a root carries no manifest, got %v", out.Manifest)
	}
	if len(out.Folders) != 1 || out.Folders[0].Name != "handbook" {
		t.Errorf("the root lists its visible children, got %+v", out.Folders)
	}
}

func TestSymlinkIsRefusedOnRead(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	link := filepath.Join(f.org, "handbook", "leak.txt")
	if err := os.Symlink(filepath.Join(f.outside, "secret.txt"), link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}
	p := problemOf(t, call(t, s, "fs_read", map[string]any{"path": link}))
	if p.Slug() != problem.SlugInvalidPath {
		t.Errorf("slug is %s, want invalid-path", p.Slug())
	}
	if p.Status != 400 {
		t.Errorf("status is %d, want 400", p.Status)
	}

	// A symlinked folder in the middle of a path is refused as well.
	linkDir := filepath.Join(f.org, "handbook", "elsewhere")
	if err := os.Symlink(f.outside, linkDir); err != nil {
		t.Fatalf("creating the folder symlink: %v", err)
	}
	p = problemOf(t, call(t, s, "fs_read", map[string]any{
		"path": filepath.Join(linkDir, "secret.txt"),
	}))
	if p.Slug() != problem.SlugInvalidPath {
		t.Errorf("slug through a linked folder is %s, want invalid-path", p.Slug())
	}
	if _, isListed := listedNames(t, s, filepath.Join(f.org, "handbook")); isListed["elsewhere"] {
		t.Error("a symlinked folder was listed as a folder of the surface")
	}
}

func TestSymlinkIsRefusedOnWrite(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	target := filepath.Join(f.outside, "secret.txt")
	link := filepath.Join(f.org, "handbook", "leak.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}
	p := problemOf(t, call(t, s, "fs_write", map[string]any{
		"path":    link,
		"content": "overwritten through a link\n",
		"message": "Follow the link",
	}))
	if p.Slug() != problem.SlugInvalidPath {
		t.Errorf("slug is %s, want invalid-path", p.Slug())
	}
	onDisk, err := os.ReadFile(target)
	if err != nil || string(onDisk) != "the crown jewels\n" {
		t.Errorf("the file outside the roots was written: %q, %v", onDisk, err)
	}
}

func TestDotComponentsAreReserved(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	write(t, filepath.Join(f.org, ".env"), "SECRET=1\n")
	cases := map[string]struct {
		tool string
		args map[string]any
	}{
		"read a dotfile": {"fs_read", map[string]any{"path": filepath.Join(f.org, ".env")}},
		"write into .git": {"fs_write", map[string]any{
			"path":    filepath.Join(f.org, "handbook", ".git", "hooks", "pre-commit"),
			"content": "#!/bin/sh\nexit 0\n",
			"message": "Install a hook",
		}},
		"list a dot folder": {"fs_list", map[string]any{
			"path": filepath.Join(f.org, "handbook", ".git"),
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := problemOf(t, call(t, s, c.tool, c.args))
			if p.Slug() != problem.SlugInvalidPath {
				t.Errorf("slug is %s, want invalid-path", p.Slug())
			}
			if p.Fix != "Names beginning with a dot are reserved." {
				t.Errorf("fix is %q", p.Fix)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(f.org, "handbook", ".git", "hooks", "pre-commit")); err == nil {
		t.Error("a hook was installed into .git")
	}
}

func TestInvisibleAncestorHidesEverythingBelowIt(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	inner := filepath.Join(f.org, "nomanifest", "inner")
	cases := map[string]map[string]any{
		"fs_list":    {"path": inner},
		"fs_read":    {"path": filepath.Join(inner, "buried.md")},
		"fs_history": {"path": filepath.Join(inner, "buried.md")},
	}
	for tool, args := range cases {
		t.Run(tool, func(t *testing.T) {
			p := problemOf(t, call(t, s, tool, args))
			if p.Slug() != problem.SlugNotVisible {
				t.Errorf("slug is %s, want not-visible", p.Slug())
			}
			if p.Status != 404 {
				t.Errorf("status is %d, want 404", p.Status)
			}
		})
	}

	t.Run("fs_write needs a visible folder", func(t *testing.T) {
		p := problemOf(t, call(t, s, "fs_write", map[string]any{
			"path":    filepath.Join(f.org, "nomanifest", "note.md"),
			"content": "smuggled\n",
			"message": "Smuggle a file in",
		}))
		if p.Slug() != problem.SlugNotVisible {
			t.Errorf("slug is %s, want not-visible", p.Slug())
		}
		if p.Fix != "Write kitbash.yaml with name and description first" {
			t.Errorf("fix is %q", p.Fix)
		}
	})

	t.Run("fs_write of a manifest below an invisible folder is refused", func(t *testing.T) {
		p := problemOf(t, call(t, s, "fs_write", map[string]any{
			"path":    filepath.Join(inner, "kitbash.yaml"),
			"content": "name: inner\ndescription: A manifest under a folder that has none.\n",
			"message": "Rewrite the buried manifest",
		}))
		if p.Slug() != problem.SlugNotVisible {
			t.Errorf("slug is %s, want not-visible", p.Slug())
		}
	})
}

func TestSchemaViolationIsProblemDetails(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	p := problemOf(t, call(t, s, "fs_write", map[string]any{
		"path":    filepath.Join(f.org, "handbook", "short.md"),
		"content": "too terse a message\n",
		"message": "a",
	}))
	if p.Slug() != problem.SlugBadRequest {
		t.Errorf("slug is %s, want bad-request", p.Slug())
	}
	if p.Status != 400 {
		t.Errorf("status is %d, want 400", p.Status)
	}
	if p.Detail == "" {
		t.Error("the validation detail was dropped")
	}
	if p.Instance != "fs_write" {
		t.Errorf("instance is %q, want fs_write", p.Instance)
	}
}

func TestWriteIntoAReadOnlyFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	f := newFixture(t)
	s := connect(t, f)

	locked := filepath.Join(f.org, "handbook", "locked")
	mkdir(t, locked)
	write(t, filepath.Join(locked, "kitbash.yaml"), `name: locked
description: A folder members may read but never write, like /org itself.
`)
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	p := problemOf(t, call(t, s, "fs_write", map[string]any{
		"path":    filepath.Join(locked, "new.md"),
		"content": "should not land\n",
		"message": "Write into a read only folder",
	}))
	if p.Slug() != problem.SlugNotPermitted {
		t.Fatalf("slug is %s, want not-permitted", p.Slug())
	}
	if p.Status != 403 {
		t.Errorf("status is %d, want 403", p.Status)
	}
	if !strings.Contains(p.Fix, "queued") {
		t.Errorf("fix is %q, want the approvals advice", p.Fix)
	}
}

func TestReadDoesNotSwallowGitFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	f := newFixture(t)
	s := connect(t, f)

	gitDir := filepath.Join(f.org, "handbook", ".git")
	if err := os.Chmod(gitDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	p := problemOf(t, call(t, s, "fs_read", map[string]any{
		"path": filepath.Join(f.org, "handbook", "README.md"),
	}))
	switch p.Slug() {
	case problem.SlugInternal, problem.SlugNotPermitted:
	default:
		t.Errorf("slug is %s, want internal or not-permitted, never a silent empty sha", p.Slug())
	}
}

// listedNames returns the folder and file names of one listing.
func listedNames(t *testing.T, s *mcp.ClientSession, path string) (map[string]bool, map[string]bool) {
	t.Helper()
	res := call(t, s, "fs_list", map[string]any{"path": path})
	ok(t, res, "fs_list")
	out := structured[fs.ListResult](t, res)
	files := map[string]bool{}
	folders := map[string]bool{}
	for _, file := range out.Files {
		files[file.Name] = true
	}
	for _, folder := range out.Folders {
		folders[folder.Name] = true
	}
	return files, folders
}

func TestReadNotFound(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	p := problemOf(t, call(t, s, "fs_read", map[string]any{
		"path": filepath.Join(f.org, "handbook", "missing.md"),
	}))
	if p.Slug() != problem.SlugNotFound {
		t.Errorf("slug is %s, want not-found", p.Slug())
	}
	if p.Status != 404 {
		t.Errorf("status is %d, want 404", p.Status)
	}
}
