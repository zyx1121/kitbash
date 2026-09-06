package daemon

import (
	"fmt"
	"net"
	"syscall"
)

// peerCred asks the kernel who opened this connection.
//
// The file descriptor comes from SyscallConn rather than File: File duplicates
// the descriptor and puts the connection into blocking mode, which breaks the
// server's read and write deadlines for the rest of its life.
func peerCred(c net.Conn) (Peer, error) {
	unix, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, ErrNoPeer
	}
	raw, err := unix.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("daemon: peer credentials: %w", err)
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Peer{}, fmt.Errorf("daemon: peer credentials: %w", err)
	}
	if credErr != nil {
		return Peer{}, fmt.Errorf("daemon: peer credentials: %w", credErr)
	}
	return Peer{PID: cred.Pid, UID: cred.Uid, GID: cred.Gid}, nil
}
