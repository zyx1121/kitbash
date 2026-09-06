//go:build !linux

package daemon

import "net"

// peerCred has no answer off Linux. kitbashd runs on the appliance, which is
// Alpine, see PLAN.md section 4.1; this file exists so the package still
// compiles on a developer's machine.
func peerCred(net.Conn) (Peer, error) { return Peer{}, ErrNoPeer }
