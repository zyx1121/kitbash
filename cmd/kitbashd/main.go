// Command kitbashd is the resident kitbash daemon: the OTLP receiver and the
// Telemetry store, see PLAN.md section 4.1. OpenRC starts it at boot as root.
// It listens on one unix socket, learns who is calling from the socket's peer
// credentials, and speaks the API of spec/kitbashd-api.yaml.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/zyx1121/kitbash/internal/daemon"
	"github.com/zyx1121/kitbash/internal/store"
)

// version is set at build time with -X main.version.
var version = "dev"

// The socket, the store and the Process receiver as spec/kitbashd-api.yaml
// declares them. The receiver binds every address because rootless networking
// delivers host.containers.internal to the host's primary address and not to
// loopback; the host firewall scopes who else may reach it, and the token
// decides whose records arrive, see PLAN.md section 4.5.
const (
	defaultSocket     = "/run/kitbash/kitbashd.sock"
	defaultStore      = "/var/lib/kitbash/kitbashd.db"
	defaultOTLPListen = "0.0.0.0:4318"
)

// Environment overrides, the same rule KITBASH_ROOTS follows for kitbash-mcp:
// they exist for tests and for running outside a kitbash host. An empty
// KITBASH_OTLP_LISTEN is not an override; pass -otlp-listen "" to serve the
// socket alone.
const (
	socketEnv     = "KITBASH_SOCKET"
	storeEnv      = "KITBASH_STORE"
	otlpListenEnv = "KITBASH_OTLP_LISTEN"
	noRestoreEnv  = "KITBASH_NO_RESTORE"
)

// SocketGroup owns the socket with root, so every member may connect and
// nobody else can, see PLAN.md section 4.5.
const socketGroup = "kitbash-users"

// Modes of what the daemon creates. The store's own modes are store.DirMode
// and store.FileMode.
const (
	socketMode = 0o660
	runDirMode = 0o755
)

var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

func main() {
	if err := run(); err != nil {
		logger.Printf("%v", err)
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", env(socketEnv, defaultSocket), "unix socket to listen on")
	storePath := flag.String("store", env(storeEnv, defaultStore), "SQLite file holding Telemetry")
	otlpListen := flag.String("otlp-listen", env(otlpListenEnv, defaultOTLPListen),
		"address of the Process receiver, empty to serve the socket alone")
	noRestore := flag.Bool("no-restore", os.Getenv(noRestoreEnv) != "",
		"do not start the registered Processes at boot")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// The store directory is the daemon's alone: it holds every member's
	// Telemetry, and the socket is the only way in. A directory that already
	// exists is narrowed too, because an upgrade from a looser layout must
	// not leave the records readable.
	storeDir := filepath.Dir(*storePath)
	if err := os.MkdirAll(storeDir, store.DirMode); err != nil {
		return fmt.Errorf("store directory %s: %w", storeDir, err)
	}
	if err := os.Chmod(storeDir, store.DirMode); err != nil {
		return fmt.Errorf("store directory %s: %w", storeDir, err)
	}
	st, err := store.Open(*storePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Printf("%v", err)
		}
	}()

	ln, err := listen(*socket)
	if err != nil {
		return err
	}
	var otlpLn net.Listener
	if *otlpListen != "" {
		otlpLn, err = net.Listen("tcp", *otlpListen)
		if err != nil {
			ln.Close()
			return fmt.Errorf("listen on %s: %w", *otlpListen, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := daemon.New(st, daemon.Options{Version: version, NoRestore: *noRestore})
	defer srv.Close()
	if _, err := srv.Sweep(ctx); err != nil {
		// A store that cannot be swept can still receive and answer, so this
		// is a warning rather than a refusal to start.
		logger.Printf("retention sweep on start failed: %v", err)
	}
	go srv.SweepLoop(ctx, daemon.SweepEvery)
	// The Processes that survived a restart are still registered, so the fan
	// out starts delivering to them again without waiting for a session.
	if err := srv.LoadSubscribers(ctx); err != nil {
		logger.Printf("could not load the fan out subscribers: %v", err)
	}

	// Both listeners stop together: whichever returns first cancels the
	// other, so a daemon that has lost one receiver does not keep serving on
	// the other as if nothing happened.
	serve, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	listeners := 1
	go func() {
		defer cancel()
		errs <- srv.Serve(serve, ln)
	}()
	if otlpLn != nil {
		listeners++
		go func() {
			defer cancel()
			errs <- srv.ServeTCP(serve, otlpLn)
		}()
		logger.Printf("listening on %s and %s, store %s, version %s", *socket, *otlpListen, *storePath, version)
	} else {
		logger.Printf("listening on %s, store %s, version %s", *socket, *storePath, version)
	}

	// Restore runs beside the listeners rather than before them: a host with
	// many containers would otherwise keep every member's session waiting on
	// podman. It logs one line when it is done, see PLAN.md section 2.3, and
	// it does nothing at all when the daemon was started with restore off.
	go srv.Restore(serve)

	var failed error
	for range listeners {
		if err := <-errs; err != nil && failed == nil {
			failed = err
		}
	}
	if failed != nil {
		return failed
	}
	logger.Printf("stopped")
	return nil
}

// env reads an override, falling back to the default of the API spec.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// listen opens the unix socket and gives it the ownership and mode
// spec/kitbashd-api.yaml declares: root:kitbash-users, 0660.
func listen(socket string) (net.Listener, error) {
	// The OpenRC script creates this directory too. Doing it here as well
	// keeps a run outside a kitbash host working.
	if err := os.MkdirAll(filepath.Dir(socket), runDirMode); err != nil {
		return nil, fmt.Errorf("socket directory %s: %w", filepath.Dir(socket), err)
	}
	if err := removeStale(socket); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socket, err)
	}
	if err := os.Chmod(socket, socketMode); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", socket, err)
	}
	chownSocket(socket)
	return ln, nil
}

// removeStale clears the socket a killed daemon left behind. Anything at that
// path that is not a socket is left alone: kitbashd does not delete files it
// did not create.
func removeStale(socket string) error {
	info, err := os.Lstat(socket)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", socket, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; move it out of the way", socket)
	}
	if err := os.Remove(socket); err != nil {
		return fmt.Errorf("remove the stale socket %s: %w", socket, err)
	}
	return nil
}

// chownSocket gives the socket to root:kitbash-users. A host without the group
// and a daemon that is not root both keep the socket as it is: the daemon
// still serves, and the operator reads why in the log.
func chownSocket(socket string) {
	group, err := user.LookupGroup(socketGroup)
	if err != nil {
		logger.Printf("group %s does not exist, so %s stays root owned; run deploy/install.sh to create it",
			socketGroup, socket)
		return
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		logger.Printf("group %s has an unreadable gid %q, so %s stays root owned", socketGroup, group.Gid, socket)
		return
	}
	if err := os.Chown(socket, 0, gid); err != nil {
		logger.Printf("could not give %s to root:%s: %v", socket, socketGroup, err)
	}
}
