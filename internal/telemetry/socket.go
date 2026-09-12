package telemetry

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"
)

// Socket is the unix socket kitbashd listens on, see PLAN.md section 4.5.
const Socket = "/run/kitbash/kitbashd.sock"

// SocketEnv overrides that path. It exists for tests and for running the
// server outside a kitbash host, so it is honoured only when the process is
// not serving an SSH session, the same rule as fs.RootsEnv.
const SocketEnv = "KITBASH_SOCKET"

// sshEnv is set by sshd on every session. It is spelled here rather than read
// from internal/fs so the fs family can call this package later without an
// import cycle.
const sshEnv = "SSH_CONNECTION"

// requestTimeout bounds one export and one JSON call. kitbashd is a local
// process on a unix socket, so a call slower than this is a call that is not
// coming back.
//
// The three calls that run a container are the exception: kitbashd is not
// answering out of its own store there, it is waiting for podman to create or
// tear down a container, which is seconds to minutes of work. They use the
// client below with their own budget, see run.go.
const requestTimeout = 5 * time.Second

// supervisorTimeout is the ceiling for a call that runs the container runtime.
// The per call budget is narrower and comes from the context; this is the
// backstop for a daemon that accepted the request and never answered.
const supervisorTimeout = 5 * time.Minute

// SocketPath is the socket this process talks to.
func SocketPath() string {
	if env := os.Getenv(SocketEnv); env != "" && os.Getenv(sshEnv) == "" {
		return env
	}
	return Socket
}

// socketClient is an HTTP client that reaches kitbashd over its unix socket.
// No host in a URL is ever resolved: every connection dials the socket.
func socketClient(socket string) *http.Client {
	return socketClientWith(socket, requestTimeout)
}

// socketClientWith is socketClient with a timeout of its own, for the calls
// that wait on the container runtime rather than on the daemon.
func socketClientWith(socket string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
			// One socket, one peer: the pooling defaults of a client that
			// talks to many hosts buy nothing here.
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}
