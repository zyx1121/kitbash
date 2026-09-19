package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// MCPPath is where the Process receiver serves MCP over streamable HTTP, so
// every Process can reach the surface as its owner, see PLAN.md section 2.3
// and mcp_for_processes in spec/kitbashd-api.yaml.
const MCPPath = "/mcp"

// MCPSessionHeader is the session identifier of the streamable HTTP transport,
// as the 2025-06-18 revision of the Model Context Protocol names it.
const MCPSessionHeader = "Mcp-Session-Id"

// MaxMCPSessions is how many live sessions one Process may hold. Every one of
// them is a kitbash-mcp running as the owner, so the count is what bounds what
// one Process costs the host.
const MaxMCPSessions = 8

// MCPIdle is how long a session may go without a request before kitbashd ends
// it and its child, see mcp_for_processes.limits in spec/kitbashd-api.yaml.
const MCPIdle = 10 * time.Minute

// CallerGrace is how long after a session ends its caller credential is still
// resolved. A child flushes its last records as it exits, and they must not
// land without the Process that recorded them.
const CallerGrace = 30 * time.Second

// DefaultMCPBinary is the kitbash-mcp a session runs. It is a flag and an
// environment override so a test can point it at a binary of its own.
const DefaultMCPBinary = "/usr/bin/kitbash-mcp"

// MCPBinaryEnv names that override.
const MCPBinaryEnv = "KITBASH_MCP_BINARY"

// mcpSessionKey carries the session one request belongs to into the SDK's
// handler, which asks for a server rather than for a session.
type mcpSessionKey struct{}

// mcpSession is one MCP session of one Process: a kitbash-mcp running as the
// Process's owner, a client connected to its stdin and stdout, and the server
// that publishes its tools back to the Process one for one.
//
// The session is the child's lifetime. Ending it closes the client, which
// closes the child's stdin and then signals it, so no session ever leaves a
// kitbash-mcp behind.
type mcpSession struct {
	registry *mcpRegistry
	process  string
	owner    string
	// credential is what the child carries as KITBASH_CALLER and records on
	// every span. kitbashd resolves it back to the Process id when the record
	// arrives, so nothing a producer sends names a Process, see the caller
	// rules in mcp_for_processes.
	credential string
	// done is closed when the session ends. Every request being served for
	// this session watches it, so an event stream held open by a client whose
	// session is over does not keep a listener from stopping.
	done chan struct{}

	// syncMu serialises publishing. Two notifications from the child can
	// arrive at once and each reads the tool set and then writes it, so
	// without it one sync could publish what the other has just removed.
	syncMu sync.Mutex

	mu sync.Mutex
	// server, client and cancel are written while the session is being
	// opened, which is after the registry already holds it: a shutdown or an
	// unregistering can end a session that is still opening, and end reads
	// exactly these three. They are written and read under the lock so that
	// ordering is the mutex's rather than the scheduler's.
	server *mcp.Server
	client *mcp.ClientSession
	cancel context.CancelFunc
	id     string
	last   time.Time
	holds  int               // requests in flight, an open event stream among them
	tools  map[string]string // tool name to the JSON of the tool as published
	ended  bool
}

// mcpRegistry holds the live sessions of every Process. It is indexed twice:
// by the session id the transport carries, and by Process, which is what the
// session cap and unregistering count.
type mcpRegistry struct {
	mu        sync.Mutex
	byID      map[string]*mcpSession
	byProcess map[string][]*mcpSession
	// byCredential maps a caller credential to the Process it names. An entry
	// outlives its session by the grace, so the records of the last batch a
	// child flushed on its way out still resolve.
	byCredential map[string]*mcpCaller
}

// mcpCaller is one caller credential: the Process it names, and when it stops
// being accepted. A zero until is a session that is still open.
type mcpCaller struct {
	process string
	until   time.Time
}

func newMCPRegistry() *mcpRegistry {
	return &mcpRegistry{
		byID:         map[string]*mcpSession{},
		byProcess:    map[string][]*mcpSession{},
		byCredential: map[string]*mcpCaller{},
	}
}

// add takes one of the Process's session slots, or reports that they are all
// taken. The slot is taken before the child starts, so eight requests arriving
// at once cannot start nine kitbash-mcp between them.
func (r *mcpRegistry) add(sess *mcpSession, max int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byProcess[sess.process]) >= max {
		return false
	}
	r.byProcess[sess.process] = append(r.byProcess[sess.process], sess)
	r.byCredential[sess.credential] = &mcpCaller{process: sess.process}
	return true
}

// caller is the Process one credential names, and false for a credential
// kitbashd did not mint or one whose grace is over.
func (r *mcpRegistry) caller(credential string, now time.Time) (string, bool) {
	if credential == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	held, known := r.byCredential[credential]
	if !known {
		return "", false
	}
	if !held.until.IsZero() && now.After(held.until) {
		return "", false
	}
	return held.process, true
}

// expire forgets every credential whose grace is over. The idle sweep calls
// it, so a daemon that runs for months does not keep one entry per session it
// ever served.
func (r *mcpRegistry) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for credential, held := range r.byCredential {
		if !held.until.IsZero() && now.After(held.until) {
			delete(r.byCredential, credential)
		}
	}
}

// name indexes a session by the id the transport gave it.
func (r *mcpRegistry) name(sess *mcpSession, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[id] = sess
}

// drop removes one session from the indexes and starts the grace on its
// credential: the child is being closed, and the records it flushed on the way
// out are still to arrive.
func (r *mcpRegistry) drop(sess *mcpSession, id string, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" {
		delete(r.byID, id)
	}
	if held, known := r.byCredential[sess.credential]; known && held.until.IsZero() {
		held.until = until
	}
	rest := r.byProcess[sess.process][:0]
	for _, held := range r.byProcess[sess.process] {
		if held != sess {
			rest = append(rest, held)
		}
	}
	if len(rest) == 0 {
		delete(r.byProcess, sess.process)
	} else {
		r.byProcess[sess.process] = rest
	}
}

// get is the session one request names, nil when kitbashd holds none.
func (r *mcpRegistry) get(id string) *mcpSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// of is every live session of one Process.
func (r *mcpRegistry) of(process string) []*mcpSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*mcpSession(nil), r.byProcess[process]...)
}

// all is every live session, which is what health counts and what closing the
// daemon ends.
func (r *mcpRegistry) all() []*mcpSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*mcpSession
	for _, sessions := range r.byProcess {
		out = append(out, sessions...)
	}
	return out
}

// count is how many sessions are live, which health reports.
func (r *mcpRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, sessions := range r.byProcess {
		n += len(sessions)
	}
	return n
}

// hold records that a request arrived and returns the release. A call that
// takes a while is not idle, so it is counted as in flight until it answers.
//
// An event stream is not counted: a client listening on a GET is waiting, not
// working, and a stream open for ten minutes with nothing on it is exactly the
// idle client mcp_for_processes.limits ends. It still ends when the session
// does, so the client learns of it rather than waiting on a stream nobody
// serves any more.
func (sess *mcpSession) hold(now time.Time, working bool) func(time.Time) {
	sess.mu.Lock()
	if working {
		sess.holds++
	}
	sess.last = now
	sess.mu.Unlock()
	return func(done time.Time) {
		sess.mu.Lock()
		if working {
			sess.holds--
		}
		sess.last = done
		sess.mu.Unlock()
	}
}

// idle reports whether nothing has arrived for this session since a moment and
// nothing is in flight.
func (sess *mcpSession) idle(before time.Time) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.holds == 0 && sess.last.Before(before)
}

// named reports whether the transport gave this session an id. A session that
// never got one cannot be addressed again, so it is ended rather than left
// holding a slot and a child.
func (sess *mcpSession) named() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.id != ""
}

// end closes the session: the slot goes back, the server session goes, and the
// caller credential starts its grace. It answers the work that is left, which
// is closing the child, or nil for a session that has already ended: a DELETE,
// an idle sweep and the child exiting can all reach it.
//
// Closing the child is separated out because it waits: the MCP specification
// ends a stdio server by closing its stdin and giving it time to exit, and a
// listener shutting down must not stand in that queue behind every session it
// was serving.
func (sess *mcpSession) end(graceUntil time.Time) (closeChild func()) {
	sess.mu.Lock()
	if sess.ended {
		sess.mu.Unlock()
		return nil
	}
	sess.ended = true
	id := sess.id
	// Read what the opening wrote while still holding the lock: a session can
	// be ended while it is still being opened, and these three are the fields
	// the two sides share.
	server, client, cancel := sess.server, sess.client, sess.cancel
	sess.mu.Unlock()

	close(sess.done)
	sess.registry.drop(sess, id, graceUntil)
	if server != nil {
		for open := range server.Sessions() {
			open.Close()
		}
	}
	return func() {
		if client != nil {
			// Closing the client closes the child's stdin, waits for it, and
			// then signals it.
			client.Close()
		}
		if cancel != nil {
			cancel()
		}
	}
}

// routesMCP registers the MCP endpoint on the Process receiver. The SDK's
// handler is built once and asks for the server of the session the guard
// resolved, so nothing reaches it without a token.
func (s *Server) routesMCP(mux *http.ServeMux) {
	s.mcpHandler = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		sess, ok := r.Context().Value(mcpSessionKey{}).(*mcpSession)
		if !ok {
			return nil
		}
		return sess.server
	}, &mcp.StreamableHTTPOptions{MaxRequestBodyBytes: otlp.MaxBodyBytes})
	mux.HandleFunc(MCPPath, s.serveMCP)
}

// serveMCP answers POST, GET and DELETE on /mcp. The token is the whole identity,
// as it is on the OTLP paths: it names the Process, and the session runs as
// that Process's owner.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost, http.MethodGet, http.MethodDelete:
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Open a session with POST, read it with GET, and end it with DELETE."))
		return
	}
	id, prob := s.tokenIdentity(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	// The SDK bounds the body too; this is the same cap read the same way, so
	// a request over it is refused before anything is decoded.
	r.Body = http.MaxBytesReader(w, r.Body, otlp.MaxBodyBytes)

	held := r.Header.Get(MCPSessionHeader)
	if held == "" {
		if r.Method != http.MethodPost {
			writeProblem(w, problem.BadRequest(r.URL.Path,
				fmt.Sprintf("%s %s carried no %s", r.Method, r.URL.Path, MCPSessionHeader),
				fmt.Sprintf("Open a session with POST first, then send its %s.", MCPSessionHeader)))
			return
		}
		// A session costs a kitbash-mcp, so only the request that opens one
		// starts a child. A client probing for a protocol this daemon does
		// not speak is answered rather than served a session it will drop.
		opens, prob := opensSession(r)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		if !opens {
			writeProblem(w, problem.BadRequest(r.URL.Path,
				fmt.Sprintf("this request carried no %s and does not open a session", MCPSessionHeader),
				fmt.Sprintf("Open a session with an initialize request, then send its %s with every later request.", MCPSessionHeader)))
			return
		}
		sess, prob := s.openMCPSession(r.Context(), id)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		// Both run however the handler leaves: a panic is turned into problem
		// details by recovered, and a session whose hold was never released
		// would never idle out and its child would never exit.
		release := sess.hold(s.now(), true)
		request, served := s.serving(r, sess)
		defer func() {
			served()
			release(s.now())
			if !sess.named() {
				// The request was not one that opens a session, or the
				// transport refused it. Nothing can address this session, so
				// it goes rather than holding a slot and a child.
				s.endMCPSession(sess)
			}
		}()
		s.mcpHandler.ServeHTTP(w, request)
		return
	}

	sess := s.mcpSessions.get(held)
	if sess == nil {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			"kitbashd holds no MCP session with that identifier",
			"Open a new session with POST; the one you held has ended."))
		return
	}
	// A token names one Process, and a session belongs to the Process that
	// opened it. Another Process's token is not a way into this one's session.
	if sess.process != id.Process {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			"that MCP session belongs to another Process",
			"Open your own session with POST."))
		return
	}
	release := sess.hold(s.now(), r.Method != http.MethodGet)
	request, served := s.serving(r, sess)
	defer func() {
		served()
		release(s.now())
		if r.Method == http.MethodDelete {
			s.endMCPSession(sess)
		}
	}()
	s.mcpHandler.ServeHTTP(w, request)
}

// serving hands the MCP handler the request with its session on it, and a
// context that ends when the session does or when the daemon stops. A GET
// holds the event stream open until one of the three happens, and without
// this a client that kept listening after its session ended would keep the
// listener from shutting down.
func (s *Server) serving(r *http.Request, sess *mcpSession) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(context.WithValue(r.Context(), mcpSessionKey{}, sess))
	go func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-sess.done:
		case <-s.stopped:
		}
	}()
	return r.WithContext(ctx), cancel
}

// opensSession reports whether a request without a session identifier is the
// one that opens a session, which is an initialize. The body is read here and
// put back, so the handler that follows reads it as it arrived.
func opensSession(r *http.Request) (bool, *problem.Problem) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return false, tooLarge(r.URL.Path)
		}
		return false, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("could not read the request body: %v", err), "Send the request again.")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false, nil
	}
	type message struct {
		Method string `json:"method"`
	}
	if trimmed[0] == '[' {
		var batch []message
		if json.Unmarshal(trimmed, &batch) != nil {
			return false, nil
		}
		for _, m := range batch {
			if m.Method == methodInitialize {
				return true, nil
			}
		}
		return false, nil
	}
	var one message
	if json.Unmarshal(trimmed, &one) != nil {
		return false, nil
	}
	return one.Method == methodInitialize, nil
}

// methodInitialize is the JSON-RPC method that opens an MCP session, as the
// Model Context Protocol names it.
const methodInitialize = "initialize"

// openMCPSession starts one kitbash-mcp as the Process's owner and publishes
// its tools. The session outlives the request that opened it, so it holds a
// context of its own.
func (s *Server) openMCPSession(ctx context.Context, id identity) (*mcpSession, *problem.Problem) {
	member, found, err := s.users.Lookup(ctx, id.User)
	if err != nil {
		return nil, problem.Internal(MCPPath, fmt.Sprintf("looking up %s: %v", id.User, err), "")
	}
	if !found {
		return nil, problem.NotPermitted(MCPPath,
			fmt.Sprintf("%s owns this Process and is no longer a member", id.User),
			"Ask an administrator to create the member again, then run the Process.")
	}

	credential, err := callerCredential()
	if err != nil {
		return nil, problem.Internal(MCPPath, err.Error(), "")
	}
	sess := &mcpSession{
		registry:   s.mcpSessions,
		process:    id.Process,
		owner:      id.User,
		credential: credential,
		done:       make(chan struct{}),
		last:       s.now(),
		tools:      map[string]string{},
	}
	if !s.mcpSessions.add(sess, MaxMCPSessions) {
		return nil, problem.TooManySessions(MCPPath,
			fmt.Sprintf("the Process %s already holds %d MCP sessions", id.Process, MaxMCPSessions),
			fmt.Sprintf("End a session with DELETE %s before opening another one.", MCPPath))
	}

	own, cancel := context.WithCancel(context.Background())
	sess.mu.Lock()
	sess.cancel = cancel
	sess.mu.Unlock()
	cmd, err := s.sessions.MCPCommand(own, member, s.mcpBinary, sess.credential)
	if err != nil {
		s.endMCPSession(sess)
		return nil, problem.Internal(MCPPath, fmt.Sprintf("building the session command: %v", err), "")
	}
	// What this Process may call, as its Package declared it at registration.
	// It is appended rather than built into the environment the runner makes,
	// because the runner speaks for a member's session and this narrows one
	// Process's: a session opened without a registration behind it is not a
	// thing that reaches here. The block is always set, so a child that finds
	// the variable empty is a child of a Process that may call nothing, not a
	// child of a daemon that forgot, see mcp_for_processes in
	// spec/kitbashd-api.yaml.
	cmd.Env = append(cmd.Env, manifest.EnvPermits+"="+string(id.Permits.JSON()))

	client := mcp.NewClient(&mcp.Implementation{Name: "kitbashd", Version: s.version}, &mcp.ClientOptions{
		// The owner may start another Process while this session is open, and
		// its tools join the surface the same way they join the owner's own,
		// see PLAN.md section 2.3. The re-sync runs beside the notification
		// because it calls the child back.
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go sess.sync(own)
		},
	})
	child, err := client.Connect(own, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		s.endMCPSession(sess)
		return nil, problem.Internal(MCPPath, fmt.Sprintf("starting %s as %s: %v", s.mcpBinary, id.User, err), "")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "kitbash", Version: s.version}, &mcp.ServerOptions{
		// What the child answered `initialize` with, forwarded the way its
		// tools are. A Process reaches this surface through kitbash-mcp like
		// its owner does, so it is told the same thing about the host and
		// there is nothing here to keep in step by hand, see PLAN.md
		// section 4.5.
		Instructions: child.InitializeResult().Instructions,
		// The transport asks for the session id right after it asks for the
		// server, which is where a session becomes addressable.
		GetSessionID: func() string {
			name := sessionID()
			sess.mu.Lock()
			sess.id = name
			sess.mu.Unlock()
			s.mcpSessions.name(sess, name)
			return name
		},
	})
	sess.mu.Lock()
	sess.client = child
	sess.server = server
	sess.mu.Unlock()
	if err := sess.sync(own); err != nil {
		s.endMCPSession(sess)
		return nil, problem.Internal(MCPPath, fmt.Sprintf("reading the tools of %s: %v", s.mcpBinary, err), "")
	}
	// A child that dies takes its session with it: the tools it answered are
	// gone, and the client the Process holds is told rather than left waiting.
	go func() {
		child.Wait()
		s.endMCPSession(sess)
	}()
	return sess, nil
}

// sync publishes the child's tools on the session's server, one for one. It
// adds what is new, replaces what changed and removes what is gone, so a
// Process the owner starts later appears without the session reconnecting.
func (sess *mcpSession) sync(ctx context.Context) error {
	if sess.client == nil || sess.server == nil {
		return nil
	}
	// One publishing at a time: the read of the tool set and the writes that
	// follow it are one change, not three.
	sess.syncMu.Lock()
	defer sess.syncMu.Unlock()

	want := map[string]*mcp.Tool{}
	for tool, err := range sess.client.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		want[tool.Name] = tool
	}

	sess.mu.Lock()
	if sess.ended {
		sess.mu.Unlock()
		return nil
	}
	var gone []string
	for name := range sess.tools {
		if _, held := want[name]; !held {
			gone = append(gone, name)
			delete(sess.tools, name)
		}
	}
	var publish []*mcp.Tool
	for name, tool := range want {
		encoded, err := json.Marshal(tool)
		if err != nil {
			logger.Printf("mcp: not publishing %s of the Process %s: %v", name, sess.process, err)
			continue
		}
		if sess.tools[name] == string(encoded) {
			continue
		}
		sess.tools[name] = string(encoded)
		publish = append(publish, tool)
	}
	sess.mu.Unlock()

	if len(gone) > 0 {
		sess.server.RemoveTools(gone...)
	}
	for _, tool := range publish {
		if err := sess.publish(tool); err != nil {
			logger.Printf("mcp: not publishing %s of the Process %s: %v", tool.Name, sess.process, err)
			sess.mu.Lock()
			delete(sess.tools, tool.Name)
			sess.mu.Unlock()
		}
	}
	return nil
}

// publish adds one proxied tool. The tool is published as the child declared
// it, schemas included, because the child is the kitbash surface and not a
// Package: what it declares is what the surface declares.
func (sess *mcpSession) publish(tool *mcp.Tool) (err error) {
	// AddTool panics on a schema it will not accept, and one tool must not
	// take the session down with it.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the tool was refused: %v", r)
		}
	}()
	proxied := *tool
	sess.server.AddTool(&proxied, sess.forward(tool.Name))
	return nil
}

// forward sends one call to the child and hands back what it answered.
// Content, structured content and the error flag cross unchanged: the child is
// the surface, and rewriting its answer here would make two surfaces.
func (sess *mcpSession) forward(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params := &mcp.CallToolParams{Name: name}
		if req.Params != nil && len(req.Params.Arguments) > 0 {
			params.Arguments = req.Params.Arguments
		}
		res, err := sess.client.CallTool(ctx, params)
		if err != nil {
			// The instance is the tool name: the Process called a tool, not a
			// path, and the tool is the only thing it can act on here.
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{
					Text: problem.Internal(name, err.Error(), "").JSON(),
				}},
			}, nil
		}
		return &mcp.CallToolResult{
			Content:           res.Content,
			StructuredContent: res.StructuredContent,
			IsError:           res.IsError,
		}, nil
	}
}

// endMCPSession ends one session and starts the grace on its caller
// credential, so the last records its child flushed still resolve. The child
// is closed beside the caller rather than in front of it; Close waits for
// those to finish, so a daemon that has stopped leaves no kitbash-mcp behind.
func (s *Server) endMCPSession(sess *mcpSession) {
	closeChild := sess.end(s.now().Add(s.callerGrace))
	if closeChild == nil {
		return
	}
	s.teardown.Add(1)
	go func() {
		defer s.teardown.Done()
		closeChild()
	}()
}

// endMCPSessions ends every session of one Process, which is what
// unregistering it does: the token that opened them is revoked, so the
// sessions it opened are over too.
func (s *Server) endMCPSessions(process string) {
	for _, sess := range s.mcpSessions.of(process) {
		s.endMCPSession(sess)
	}
}

// closeMCPSessions ends every session the daemon holds, which is what stopping
// it does. It returns as soon as the sessions are ended; waitForChildren is
// what waits for the children to go.
func (s *Server) closeMCPSessions() {
	for _, sess := range s.mcpSessions.all() {
		s.endMCPSession(sess)
	}
}

// waitForChildren waits for the kitbash-mcp of every ended session to exit,
// bounded the same way an in flight request is. A child that outlasts it is
// left to the signals its transport is already sending, and the operator is
// told rather than the daemon hanging on the way out.
func (s *Server) waitForChildren() {
	done := make(chan struct{})
	go func() {
		s.teardown.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(ShutdownTimeout):
		logger.Printf("mcp: a session child had not exited after %s; it is being signalled", ShutdownTimeout)
	}
}

// sweepMCPSessions ends every session that has gone idle. A client that stops
// asking is a client that has gone away, and its child must not outlive it.
func (s *Server) sweepMCPSessions(now time.Time) int {
	ended := 0
	for _, sess := range s.mcpSessions.all() {
		if sess.idle(now.Add(-s.mcpIdle)) {
			logger.Printf("mcp: ending an idle session of the Process %s", sess.process)
			s.endMCPSession(sess)
			ended++
		}
	}
	// The credentials of the sessions that ended before this one are given
	// their grace and then forgotten.
	s.mcpSessions.expire(now)
	return ended
}

// mcpSweepLoop ends idle sessions until the daemon stops. It ticks well inside
// the idle window so a session ends near its deadline rather than a window
// later.
func (s *Server) mcpSweepLoop() {
	every := s.mcpIdle / 4
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	if every > time.Minute {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
			s.sweepMCPSessions(s.now())
		}
	}
}

// sessionID is the identifier the transport carries. It is random rather than
// sequential: it is the only thing a request presents besides its token, and
// guessing another Process's session must not be possible.
func sessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux, and a session without an
		// unguessable id is one this daemon will not open.
		panic(fmt.Sprintf("daemon: read random bytes: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// callerCredential is what one session's child carries as KITBASH_CALLER: 32
// random bytes, the same entropy as a Process token, and meaningful only to
// the daemon that minted it. It is not the Process id, so a producer that
// repeats what it saw on a record names nothing.
func callerCredential() (string, error) {
	var b [callerCredentialBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("daemon: mint a caller credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// callerCredentialBytes is the entropy of one caller credential.
const callerCredentialBytes = 32

// mcpBinaryPath is the kitbash-mcp a session runs: what the operator named,
// what the environment names, or the path spec/kitbashd-api.yaml declares.
func mcpBinaryPath(configured string) string {
	if configured != "" {
		return configured
	}
	if env := os.Getenv(MCPBinaryEnv); env != "" {
		return env
	}
	return DefaultMCPBinary
}

// mcpSessionsOf is the Sessions implementation a daemon uses when its options
// name none: the same setuid runner that starts containers.
func mcpSessionsOf(runner sysusers.Runner) sysusers.Sessions {
	if sessions, ok := runner.(sysusers.Sessions); ok {
		return sessions
	}
	return sysusers.NewPodman()
}
