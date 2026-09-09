package daemon

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// bearerPrefix is how a Process presents its token on the TCP receiver.
const bearerPrefix = "bearer "

// BearerChallenge is the WWW-Authenticate a 401 carries, as RFC 9110 requires
// of every 401.
const BearerChallenge = `Bearer realm="kitbash Process receiver"`

// identity is what a record is stamped with: who it is about, and who wrote
// it. A session over the unix socket is the member on both counts; a Process
// over the TCP receiver is its owner and its own id, see PLAN.md section 2.4.
type identity struct {
	// User is kitbash.user: the member the record is about.
	User string
	// Package and Process are stamped only for a Process record; a session
	// sends its own and they are kept as sent.
	Package string
	Process string
	// Producer is kitbash.producer: the member name or the Process id.
	Producer string
	// EvalClaims is true when this producer's evaluation records may carry
	// their subject's identity. Only a Process an admin runs may, see
	// PLAN.md section 2.4 Evaluation.
	EvalClaims bool
	// Permits is what this Process may call over /mcp, as its Package's
	// manifest declared it at registration. It is read only by the MCP
	// endpoint, which hands it to the session child; a record carries none.
	Permits manifest.Permits
	// Callers resolves the caller credential a record carries to the Process
	// whose MCP session recorded it. It is nil for a producer that may not
	// carry one at all, which is every Process on the TCP receiver: only a
	// session kitbashd started has a credential, and it reaches the daemon
	// over the socket.
	Callers func(credential string) (string, bool)
}

// identifier resolves who is on the other end of one request. There are two:
// the socket's peer credentials, and a Process token on the TCP receiver.
type identifier func(r *http.Request) (identity, *problem.Problem)

// apply stamps one record's attributes.
//
// The normal case replaces what the producer sent: kitbash.user is never
// trusted from a producer, and a Process's package and process come from the
// token, not the body. An evaluation record from an admin's kit is the one
// exception, and even there the producer is stamped, so a judgment is never
// mistaken for the act it judges.
func (id identity) apply(a *store.Attributes) {
	// kitbash.caller is never taken from a producer either. What a session
	// sends is the credential kitbashd gave its child, and only kitbashd can
	// say which Process that names; a value it did not mint, or one whose
	// session ended, names nothing and is dropped rather than stored.
	a.Caller = id.caller(a.Caller)

	if id.EvalClaims && a.Eval != nil && *a.Eval {
		// The claims are about the subject of the judgment. Whatever the kit
		// left out falls back to the kit's own identity, so no record ever
		// reaches the store without a user.
		if a.User == "" {
			a.User = id.User
		}
		if a.Package == "" {
			a.Package = id.Package
		}
		if a.Process == "" {
			a.Process = id.Process
		}
		a.Producer = id.Producer
		return
	}
	a.User = id.User
	if id.Package != "" {
		a.Package = id.Package
	}
	if id.Process != "" {
		a.Process = id.Process
	}
	a.Producer = id.Producer
}

// caller resolves the credential on a record to the Process it names, and
// answers the empty string for anything else. A member exporting a credential
// they guessed, a Process repeating one it read off a record, and an SSH
// session with KITBASH_CALLER set by hand all land here and are dropped.
func (id identity) caller(credential string) string {
	if credential == "" || id.Callers == nil {
		return ""
	}
	process, known := id.Callers(credential)
	if !known {
		return ""
	}
	return process
}

// socketIdentity reads the member behind a unix socket connection. The member
// is both the subject and the producer of everything they export.
func (s *Server) socketIdentity(r *http.Request) (identity, *problem.Problem) {
	caller, prob := s.caller(r)
	if prob != nil {
		return identity{}, prob
	}
	return identity{
		User:     caller.User,
		Producer: caller.User,
		// The kitbash-mcp of a Process session is a member session like any
		// other: it exports over this socket as its owner, and its credential
		// is what tells the two apart.
		Callers: s.mcpCaller,
	}, nil
}

// mcpCaller resolves one caller credential against the live sessions and the
// ones still inside their grace.
func (s *Server) mcpCaller(credential string) (string, bool) {
	return s.mcpSessions.caller(credential, s.now())
}

// tokenIdentity reads the Process behind a request to the TCP receiver. A
// container's address says nothing about who runs it, so the token is the
// whole identity, see PLAN.md section 4.5.
func (s *Server) tokenIdentity(r *http.Request) (identity, *problem.Problem) {
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		return identity{}, problem.NotAuthenticated(r.URL.Path,
			"this request carried no Process token", "")
	}
	p, found, err := s.store.ProcessByToken(r.Context(), token)
	if err != nil {
		return identity{}, problem.Internal(r.URL.Path, err.Error(), "")
	}
	if !found {
		return identity{}, problem.NotAuthenticated(r.URL.Path,
			"this token names no Process kitbashd is running",
			"Run the Process again to mint a token; the one it holds was revoked.")
	}
	// No Callers: a Process exports as itself over this listener, and
	// kitbash.process already says which one. A caller on a record here could
	// only be a claim, so it is dropped.
	return identity{
		User:       p.Owner,
		Package:    p.Package,
		Process:    p.ID,
		Producer:   p.ID,
		EvalClaims: p.Admin,
		Permits:    p.Permits,
	}, nil
}

// bearer reads the token out of an Authorization header. The scheme is
// compared without case, as RFC 9110 requires.
func bearer(header string) (string, bool) {
	if len(header) < len(bearerPrefix) {
		return "", false
	}
	if !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" || len(token) > maxTokenBytes {
		return "", false
	}
	return token, true
}

// maxTokenBytes bounds what the receiver will hash. A kitbashd token is 43
// characters; anything far longer is not one, and hashing whatever a caller
// sends is work an unauthenticated caller should not be able to ask for.
const maxTokenBytes = 512

// notServedOverTCP is the answer to every path the Process receiver does not
// serve. The /kitbash/v1 API is socket only, see spec/kitbashd-api.yaml.
func (s *Server) notServedOverTCP(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, problem.NotFoundFix(r.URL.Path,
		fmt.Sprintf("%s is not served on the Process receiver", r.URL.Path),
		fmt.Sprintf("Export Telemetry to %s, %s or %s, or open an MCP session at %s; the rest of the API is on the kitbashd socket.",
			pathTraces, pathLogs, pathMetrics, MCPPath)))
}

// The three OTLP paths, the only ones the Process receiver serves.
const (
	pathTraces  = "/v1/traces"
	pathLogs    = "/v1/logs"
	pathMetrics = "/v1/metrics"
)

// subjectOf reads the subject of an evaluation record out of the attributes
// the producer sent. Both keys stay in Other; this is how the query surface
// publishes them, see the telAttributes schema in spec/mcp-surface.yaml.
func subjectOf(other map[string]any) *subject {
	traceID, _ := other[otlp.AttrSubjectTraceID].(string)
	spanID, _ := other[otlp.AttrSubjectSpanID].(string)
	if traceID == "" && spanID == "" {
		return nil
	}
	return &subject{TraceID: traceID, SpanID: spanID}
}
