package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
)

// Peer is what the kernel says about the other end of the socket. It is the
// whole identity story: there is no token and no second identity, see PLAN.md
// section 4.5.
type Peer struct {
	PID int32
	UID uint32
	GID uint32
}

// peerContextKey carries the peer from the accept loop to the handler.
type peerContextKey struct{}

// ErrNoPeer reports a connection whose peer credentials could not be read,
// which on a unix socket means it is not a unix socket.
var ErrNoPeer = errors.New("daemon: no peer credentials on this connection")

// withPeer is the ConnContext of the HTTP server. The credentials were read
// when the connection was accepted, which is early enough: the kernel fixes
// them when the peer connects and they never change after that.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	accepted, ok := c.(*acceptedConn)
	if !ok || !accepted.known {
		return ctx
	}
	return context.WithValue(ctx, peerContextKey{}, accepted.peer)
}

// acceptedConn is one connection the daemon accepted: who opened it, and the
// slot in the connection cap it gives back when it closes.
type acceptedConn struct {
	net.Conn
	peer    Peer
	known   bool
	release func()
	once    sync.Once
}

// Close frees the slot before closing, and only ever frees it once, however
// many times the server closes the connection.
func (c *acceptedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// limitListener accepts at most a fixed number of connections at once and
// reads each one's peer credentials as it arrives.
//
// The credentials are read here rather than in ConnContext because they can
// only be read from the *net.UnixConn itself, and anything that wraps a
// connection hides that type. Reading at accept time means one place knows how
// a connection becomes an identity.
type limitListener struct {
	net.Listener
	slots chan struct{}
}

// limit wraps ln so no more than n connections are served at once.
func limit(ln net.Listener, n int) net.Listener {
	slots := make(chan struct{}, n)
	for range n {
		slots <- struct{}{}
	}
	return &limitListener{Listener: ln, slots: slots}
}

func (l *limitListener) Accept() (net.Conn, error) {
	<-l.slots
	conn, err := l.Listener.Accept()
	if err != nil {
		l.slots <- struct{}{}
		return nil, err
	}
	peer, credErr := peerCred(conn)
	return &acceptedConn{
		Conn:    conn,
		peer:    peer,
		known:   credErr == nil,
		release: func() { l.slots <- struct{}{} },
	}, nil
}

// peerFrom reads the credentials the accept loop stored.
func peerFrom(ctx context.Context) (Peer, error) {
	peer, ok := ctx.Value(peerContextKey{}).(Peer)
	if !ok {
		return Peer{}, ErrNoPeer
	}
	return peer, nil
}
