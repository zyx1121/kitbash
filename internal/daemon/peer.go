package daemon

import (
	"context"
	"errors"
	"net"
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

// withPeer is the ConnContext of the HTTP server. Reading the credentials once
// per connection is enough: the kernel fixes them when the peer connects.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	peer, err := peerCred(c)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, peerContextKey{}, peer)
}

// peerFrom reads the credentials the accept loop stored.
func peerFrom(ctx context.Context) (Peer, error) {
	peer, ok := ctx.Value(peerContextKey{}).(Peer)
	if !ok {
		return Peer{}, ErrNoPeer
	}
	return peer, nil
}
