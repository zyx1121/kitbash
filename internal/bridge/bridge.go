// Package bridge puts the tools of a running Process with expose: mcp on the
// caller's own MCP surface, as PLAN.md section 2.3 describes.
//
// The image's entrypoint is a stdio MCP server running as PID 1 with stdin
// held open. Every session execs one more instance of it inside the container
// and proxies calls to that, exactly the way an agent runs a stdio server on a
// laptop. The tools published are the ones the manifest declares, with the
// manifest's schemas, so a tool the server offers but the manifest does not is
// not on the surface.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
)

// ImportHook is the lifecycle hook and the tool name an import kit declares,
// see PLAN.md section 3.
const (
	ImportHook = "import"
	ImportTool = "import"
)

// Transport opens the connection to one Process. The default runs
// podman exec inside its container; tests substitute an in memory pair.
type Transport func(ctx context.Context, p *proc.Process) (mcp.Transport, error)

// Kit is a running Process that implements the import hook, with the resolved
// input schema of its import tool. pkg_import picks between kits by validating
// the source against these schemas, never by name.
type Kit struct {
	Process *proc.Process
	Tool    manifest.Tool
}

// Bridge publishes Package tools on one MCP server and forwards their calls.
type Bridge struct {
	files     *fs.Service
	processes *proc.Service
	runner    podman.Runner
	transport Transport
	logger    *log.Logger

	mu        sync.Mutex
	server    *mcp.Server
	published map[string][]string           // Process id to the surface tool names it added
	owners    map[string]string             // surface tool name to the Process id that answers it
	packages  map[string]string             // Process id to the Package path it runs
	sessions  map[string]*mcp.ClientSession // Process id to its open session
}

// New builds a bridge over one caller's Processes.
func New(files *fs.Service, processes *proc.Service, runner podman.Runner) *Bridge {
	b := &Bridge{
		files:     files,
		processes: processes,
		runner:    runner,
		logger:    log.New(os.Stderr, "kitbash: ", log.LstdFlags),
		published: map[string][]string{},
		owners:    map[string]string{},
		packages:  map[string]string{},
		sessions:  map[string]*mcp.ClientSession{},
	}
	b.transport = b.execTransport
	return b
}

// Attach binds the bridge to the server it publishes on. server.New calls it.
func (b *Bridge) Attach(s *mcp.Server) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.server = s
}

// SetLogger replaces where the bridge reports what it could not publish and
// what it had to register again. Tests read those lines; the server leaves it
// at stderr, which is the session's own log.
func (b *Bridge) SetLogger(logger *log.Logger) {
	if logger == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logger = logger
}

// SetTransport replaces how the bridge reaches a Process. Tests use it to run
// the Package in process instead of in a container.
func (b *Bridge) SetTransport(t Transport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transport = t
}

// Sync publishes the tools of every running Process of the caller that
// declares expose: mcp. The server calls it once when a session starts; a
// Process started in another session appears when this one reconnects, which
// is the decision recorded in PLAN.md section 5.5.
func (b *Bridge) Sync(ctx context.Context) {
	list, prob := b.processes.List(ctx)
	if prob != nil {
		b.logger.Printf("bridge: listing Processes: %s", prob.Detail)
		return
	}
	b.reconcile(ctx, list.Processes)
	for i := range list.Processes {
		p := list.Processes[i]
		if p.State != proc.StateRunning || p.Expose != manifest.ExposeMCP {
			continue
		}
		if prob := b.Add(ctx, &p); prob != nil {
			// A Package whose folder went out of the surface, whose manifest
			// now names a built in family, or whose names another Process
			// already answers is skipped, not fatal: the rest of the surface
			// still works, and the reason is in the server log.
			b.logger.Printf("bridge: skipping Process %s at %s: %s", p.ID, p.Package, prob.Detail)
		}
	}
}

// reconcile tells kitbashd about the Processes that are running here and that
// it does not know, which is how a restarted daemon learns the running set,
// see client_behaviour.processes in spec/kitbashd-api.yaml. A re-registered
// Process holds the token from before, so its own exports stay refused until
// it is run again; that is said plainly in the log rather than hidden.
func (b *Bridge) reconcile(ctx context.Context, running []proc.Process) {
	registered, stale, prob := b.processes.Reconcile(ctx, running)
	if prob != nil {
		b.logger.Printf("bridge: reading the Process registry: %s", prob.Detail)
		return
	}
	for _, id := range registered {
		b.logger.Printf("bridge: re-registered %s; its exports resume when it is run again", id)
	}
	for _, id := range stale {
		b.logger.Printf("bridge: kitbashd still holds Process %s, which no longer runs here", id)
	}
}

// Add publishes the tools of one Process. It is called after proc_run.
func (b *Bridge) Add(ctx context.Context, p *proc.Process) *problem.Problem {
	if p == nil || p.Expose != manifest.ExposeMCP || p.State != proc.StateRunning {
		return nil
	}
	m, folder, prob := b.files.Manifest(ctx, p.Package)
	if prob != nil {
		return prob
	}
	// proc_run refuses a reserved Package name, but a manifest can be rewritten
	// after the Process started, and this session reads the manifest as it is
	// now. A Package that renamed itself into a built in family would take
	// fs_list off the surface, so the check belongs here too.
	if manifest.Reserved(m.Name) {
		return problem.InvalidManifest(folder, fmt.Sprintf(
			"the Package name %q is a built in tool family, so its tools cannot join the surface", m.Name))
	}
	tools, err := b.schemas(m, folder)
	if err != nil {
		return problem.InvalidManifest(folder, err.Error())
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.server == nil {
		return problem.Internal(p.Package, "the bridge is not attached to a server", "")
	}
	// This Process gives up the names it held before it publishes again, so a
	// second Add for the same Process never collides with itself.
	b.remove(p.ID)
	var names []string
	// Two different things can keep a tool off the surface, and they have
	// different answers: someone else answers that name already, or the tool's
	// own schema is one the surface cannot publish.
	var taken, unusable []string
	for _, tool := range tools {
		surface := proc.ToolName(m.Name, tool.Name)
		if reservedSurface(surface) {
			b.logger.Printf("bridge: not publishing %s: the name is inside a built in tool family", surface)
			taken = append(taken, surface)
			continue
		}
		if owner, held := b.owners[surface]; held && owner != p.ID {
			b.logger.Printf("bridge: not publishing %s: Process %s already answers it", surface, owner)
			taken = append(taken, surface)
			continue
		}
		if err := b.addTool(p, surface, tool); err != nil {
			b.logger.Printf("bridge: not publishing %s: %v", surface, err)
			unusable = append(unusable, tool.Name)
			continue
		}
		b.owners[surface] = p.ID
		names = append(names, surface)
	}
	b.published[p.ID] = names
	b.packages[p.ID] = p.Package
	// The instance is the Process itself: the caller can stop it with the id
	// in front of them, without looking it up first.
	if len(taken) > 0 {
		return problem.ConflictFix(p.ID, fmt.Sprintf(
			"the Process at %s is running, but %s could not join the surface because those names are already answered",
			p.Package, strings.Join(taken, ", ")),
			"Stop the Process that answers those names with proc_stop, or rename this Package, then run it again.")
	}
	if len(unusable) > 0 {
		return problem.InvalidManifestFix(p.ID, fmt.Sprintf(
			"the Process at %s is running, but the input schema of %s is not one the surface can publish",
			p.Package, strings.Join(unusable, ", ")),
			"Give each tool an input schema of type object that JSON Schema 2020-12 accepts, then run it again.")
	}
	return nil
}

// reservedSurface reports whether a surface name sits inside a built in tool
// family. Package names carry no underscore, so the first underscore is the
// split between the namespace and the tool, see spec/mcp-surface.yaml.
func reservedSurface(surface string) bool {
	namespace, _, found := strings.Cut(surface, "_")
	return !found || manifest.Reserved(namespace)
}

// Remove unpublishes the tools of one Process and closes its session. It is
// called after proc_stop and after a run replaced an older Process.
func (b *Bridge) Remove(p *proc.Process) {
	if p == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remove(p.ID)
}

// Close ends every session the bridge opened. kitbash-mcp calls it when the
// SSH session ends.
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.sessions {
		b.closeSession(id)
	}
}

// remove drops one Process from the surface. It unpublishes only the names
// this Process owns: another Process may have taken a name since, and removing
// it would take that one off the surface instead. The caller holds the lock.
func (b *Bridge) remove(id string) {
	var mine []string
	for _, surface := range b.published[id] {
		if b.owners[surface] != id {
			continue
		}
		delete(b.owners, surface)
		mine = append(mine, surface)
	}
	if len(mine) > 0 && b.server != nil {
		b.server.RemoveTools(mine...)
	}
	delete(b.published, id)
	delete(b.packages, id)
	b.closeSession(id)
}

// Owner is the Package path and the Process id that answer one surface tool
// name, and false when the name is a built in tool. The tracing middleware
// asks so the span of a proxied call carries kitbash.package and
// kitbash.process without the Package having to send them.
func (b *Bridge) Owner(surface string) (path, process string, ok bool) {
	if b == nil {
		return "", "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id, held := b.owners[surface]
	if !held {
		return "", "", false
	}
	return b.packages[id], id, true
}

// closeSession ends the client session for one Process. The caller holds the
// lock.
func (b *Bridge) closeSession(id string) {
	if session, ok := b.sessions[id]; ok {
		session.Close()
		delete(b.sessions, id)
	}
}

// Tools are the surface names currently published for a Process.
func (b *Bridge) Tools(id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published[id]...)
}

// schemas reads the manifest's tool schemas from the Package folder. The
// folder is named below the root it belongs to rather than treated as a root
// of its own, so every component from the root down is resolved in one
// openat2: a folder above the Package swapped for a symlink mid session cannot
// feed a schema from somewhere else into the surface.
func (b *Bridge) schemas(m *manifest.Manifest, folder string) ([]manifest.Tool, error) {
	root, rel, ok := b.files.RootOf(folder)
	if !ok {
		// fs resolved this folder a moment ago, so this is a folder that left
		// the roots underneath us. Refuse rather than reach for it.
		return nil, fmt.Errorf("%s is outside the roots", folder)
	}
	return m.ResolveSchemasBelow(root, rel)
}

// addTool registers one proxied tool. The manifest's input schema becomes the
// tool's input schema, so the SDK validates arguments before the call reaches
// the container. No output schema is declared: the Package is not trusted to
// honour it, so the bridge validates the result itself and says so plainly
// when the Package broke its own contract.
func (b *Bridge) addTool(p *proc.Process, surface string, tool manifest.Tool) (err error) {
	input, marshalErr := json.Marshal(tool.Input)
	if marshalErr != nil {
		return fmt.Errorf("the input schema is not representable as JSON: %w", marshalErr)
	}
	if kind, _ := tool.Input["type"].(string); kind != "object" {
		return errors.New(`the input schema must have type "object"`)
	}
	// AddTool panics on a schema it cannot resolve, and one Package must not
	// take the surface down with it.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the input schema was refused: %v", r)
		}
	}()
	process := *p
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        surface,
		Description: tool.Description,
		InputSchema: json.RawMessage(input),
	}, b.handler(&process, tool, surface))
	return nil
}

// handler forwards one tool call to the Package and validates what comes back.
func (b *Bridge) handler(p *proc.Process, tool manifest.Tool, surface string) mcp.ToolHandlerFor[json.RawMessage, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in json.RawMessage) (*mcp.CallToolResult, any, error) {
		result, prob := b.call(ctx, p, tool, surface, in)
		if prob != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: prob.JSON()}},
			}, nil, nil
		}
		return result, nil, nil
	}
}

// call forwards one call and returns the Package's own result.
func (b *Bridge) call(ctx context.Context, p *proc.Process, tool manifest.Tool, surface string, in json.RawMessage) (*mcp.CallToolResult, *problem.Problem) {
	session, prob := b.session(ctx, p)
	if prob != nil {
		return nil, prob
	}
	var args any = in
	if len(in) == 0 {
		args = map[string]any{}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
	if err != nil {
		// The Package may have died under us; the next call reconnects.
		b.mu.Lock()
		b.closeSession(p.ID)
		b.mu.Unlock()
		return nil, problem.Internal(surface, err.Error(), "")
	}
	if res.IsError {
		// A Package that already answers in problem details is answering in
		// the surface's own language, so its problem is returned as it stands:
		// a kit's not-found reaches the agent as a not-found instead of a
		// bad-request with JSON buried in the detail, see issue #58. Every
		// other error text is a Package reporting in its own words, and that
		// is what the wrapper is for.
		if passed := packageProblem(res); passed != nil {
			return nil, passed
		}
		return nil, problem.BadRequest(surface, remoteText(res),
			"The Package refused the call; read the detail.")
	}
	if res.StructuredContent == nil {
		// A tool that answers in prose is answering, not misbehaving. There is
		// nothing to validate against the manifest, so the content goes
		// through as it came.
		return &mcp.CallToolResult{Content: res.Content}, nil
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, problem.InternalDetail(surface, err.Error(),
			"the Package returned output that does not match its manifest",
			"Ask the Package's author to return the output its manifest declares.")
	}
	if err := tool.ValidateOutput(structured); err != nil {
		return nil, problem.InternalDetail(surface, err.Error(),
			"the Package returned output that does not match its manifest",
			"Ask the Package's author to return the output its manifest declares.")
	}
	return &mcp.CallToolResult{
		Content:           res.Content,
		StructuredContent: json.RawMessage(structured),
	}, nil
}

// session returns the open session for one Process, opening it on first use.
// One session lives for as long as kitbash-mcp does, the way an agent keeps
// one stdio server running for a whole conversation.
func (b *Bridge) session(ctx context.Context, p *proc.Process) (*mcp.ClientSession, *problem.Problem) {
	b.mu.Lock()
	if session, ok := b.sessions[p.ID]; ok {
		b.mu.Unlock()
		return session, nil
	}
	transport := b.transport
	b.mu.Unlock()

	// The session outlives the call that opened it, so it does not inherit
	// that call's cancellation.
	own := context.WithoutCancel(ctx)
	t, err := transport(own, p)
	if err != nil {
		return nil, problem.Internal(p.Package, err.Error(), "")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kitbash", Version: "1"}, nil)
	session, err := client.Connect(own, t, nil)
	if err != nil {
		return nil, problem.Internal(p.Package, err.Error(),
			"Check the Process is running with proc_list and read proc_logs.")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if open, ok := b.sessions[p.ID]; ok {
		// Another call opened one first; keep that one.
		session.Close()
		return open, nil
	}
	b.sessions[p.ID] = session
	return session, nil
}

// execTransport runs the image's own entrypoint inside the Process's
// container, with stdin held open.
func (b *Bridge) execTransport(ctx context.Context, p *proc.Process) (mcp.Transport, error) {
	entrypoint, cmd, err := b.runner.ImageEntrypoint(ctx, p.Digest)
	if err != nil {
		return nil, err
	}
	argv := append(append([]string{}, entrypoint...), cmd...)
	if len(argv) == 0 {
		return nil, fmt.Errorf("the image of %s declares no entrypoint to exec", p.Package)
	}
	full := b.runner.ExecArgv(p.Container, argv)
	return &mcp.CommandTransport{Command: exec.CommandContext(ctx, full[0], full[1:]...)}, nil
}

// Kits are the caller's running import kits: Processes whose manifest declares
// provides.kit with import and a tool named import.
func (b *Bridge) Kits(ctx context.Context) ([]Kit, *problem.Problem) {
	list, prob := b.processes.List(ctx)
	if prob != nil {
		return nil, prob
	}
	var kits []Kit
	for i := range list.Processes {
		p := list.Processes[i]
		if p.State != proc.StateRunning || p.Expose != manifest.ExposeMCP {
			continue
		}
		m, folder, prob := b.files.Manifest(ctx, p.Package)
		if prob != nil {
			b.logger.Printf("bridge: skipping kit at %s: %s", p.Package, prob.Detail)
			continue
		}
		if !m.HasKit(ImportHook) {
			continue
		}
		tools, err := b.schemas(m, folder)
		if err != nil {
			b.logger.Printf("bridge: skipping kit at %s: %v", p.Package, err)
			continue
		}
		for _, tool := range tools {
			if tool.Name != ImportTool {
				continue
			}
			kits = append(kits, Kit{Process: &p, Tool: tool})
			break
		}
	}
	return kits, nil
}

// CallTool calls one tool of a Process and returns its structured content.
// pkg_import uses it to run an import kit.
func (b *Bridge) CallTool(ctx context.Context, p *proc.Process, tool string, args map[string]any) (json.RawMessage, *problem.Problem) {
	session, prob := b.session(ctx, p)
	if prob != nil {
		return nil, prob
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		b.mu.Lock()
		b.closeSession(p.ID)
		b.mu.Unlock()
		return nil, problem.Internal(p.Package, err.Error(), "")
	}
	if res.IsError {
		return nil, problem.BadRequest(p.Package, remoteText(res),
			"Read the kit's own error and call pkg_import again with a source it accepts.")
	}
	if res.StructuredContent != nil {
		encoded, err := json.Marshal(res.StructuredContent)
		if err == nil {
			return encoded, nil
		}
	}
	// A Package that returns only a text block still has to return JSON.
	text := remoteText(res)
	if json.Valid([]byte(text)) {
		return json.RawMessage(text), nil
	}
	return nil, problem.InternalDetail(p.Package,
		fmt.Sprintf("%s returned no structured content", tool),
		"the Package returned output that does not match its manifest",
		"Ask the Package's author to return the output its manifest declares.")
}

// remoteText is the text a Package returned, for the detail of a problem.
func remoteText(res *mcp.CallToolResult) string {
	var parts []string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	joined := strings.TrimSpace(strings.Join(parts, "\n"))
	if joined == "" {
		return "the Package reported an error with no message"
	}
	return joined
}
