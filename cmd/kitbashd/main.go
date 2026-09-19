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
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/zyx1121/kitbash/internal/daemon"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/secrets"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// version is set at build time with -X main.version.
var version = "dev"

// The socket, the store and the Process receiver as spec/kitbashd-api.yaml
// declares them. The receiver binds every address because rootless networking
// delivers host.containers.internal to the host's primary address and not to
// loopback; deploy/install.sh writes the nftables ruleset that keeps it to the
// loopback interface, which is where pasta delivers a Process, and the token
// decides whose records arrive, see PLAN.md section 4.5.
const (
	defaultSocket     = "/run/kitbash/kitbashd.sock"
	defaultStore      = "/var/lib/kitbash/kitbashd.db"
	defaultOTLPListen = "0.0.0.0:4318"
)

// The reverse proxy, as PLAN.md section 2.3 describes it. A host with no
// domain binds neither of these: nothing is routed and an http Process keeps
// its internal port, which is what every kitbash host did before the proxy
// existed. With a domain, acme binds both and obtains certificates, gateway
// binds 80 alone and trusts the gateway in front of it.
const (
	defaultProxyListen    = daemon.DefaultProxyListen
	defaultProxyTLSListen = daemon.DefaultProxyTLSListen
	defaultCerts          = daemon.DefaultCertDir
)

// defaultSecrets is where the values members set with secrets_set are kept, as
// spec/kitbashd-api.yaml declares them. The daemon creates the directory root
// owned and 0700 at start, so no installer has to, see internal/secrets.
const defaultSecrets = secrets.DefaultDir

// defaultMCPBinary is the kitbash-mcp one MCP session of a Process runs, as
// mcp_for_processes in spec/kitbashd-api.yaml declares it.
const defaultMCPBinary = daemon.DefaultMCPBinary

// Environment overrides, the same rule KITBASH_ROOTS follows for kitbash-mcp:
// they exist for tests and for running outside a kitbash host. An empty
// KITBASH_OTLP_LISTEN is not an override; pass -otlp-listen "" to serve the
// socket alone.
const (
	socketEnv     = "KITBASH_SOCKET"
	storeEnv      = "KITBASH_STORE"
	secretsEnv    = "KITBASH_SECRETS"
	otlpListenEnv = "KITBASH_OTLP_LISTEN"
	noRestoreEnv  = "KITBASH_NO_RESTORE"
	mcpBinaryEnv  = daemon.MCPBinaryEnv
	// The proxy's own settings. deploy/install.sh writes the first two into
	// /etc/conf.d/kitbashd, which the OpenRC service exports, so the operator
	// gives the host a domain once and every boot after that has it.
	domainEnv         = "KITBASH_DOMAIN"
	tlsEnv            = "KITBASH_TLS"
	proxyListenEnv    = "KITBASH_PROXY_LISTEN"
	proxyTLSListenEnv = "KITBASH_PROXY_TLS_LISTEN"
	certsEnv          = "KITBASH_CERTS"
	// publicAddressEnv is the address the proxy is reached on from outside,
	// which is what a unit's declared hostname is proved against. In gateway
	// mode the host's own addresses are behind the gateway, so the operator
	// names the public one here.
	publicAddressEnv = daemon.PublicAddressEnv
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
	secretsDir := flag.String("secrets", env(secretsEnv, defaultSecrets),
		"directory holding the values members set with secrets_set, one root owned file per name")
	otlpListen := flag.String("otlp-listen", env(otlpListenEnv, defaultOTLPListen),
		"address of the Process receiver, empty to serve the socket alone")
	mcpBinary := flag.String("mcp-binary", env(mcpBinaryEnv, defaultMCPBinary),
		"kitbash-mcp to run as the owner for each MCP session of a Process")
	noRestore := flag.Bool("no-restore", truthy(os.Getenv(noRestoreEnv)),
		"do not start the registered Processes at boot")
	domain := flag.String("domain", env(domainEnv, ""),
		"domain every http Process is served under, empty for a host that routes nothing")
	tlsMode := flag.String("tls", env(tlsEnv, daemon.TLSACME),
		"how this host terminates TLS: acme obtains its own certificates, gateway trusts the one in front of it")
	proxyListen := flag.String("proxy-listen", env(proxyListenEnv, defaultProxyListen),
		"address the reverse proxy serves HTTP on, used only when a domain is set")
	proxyTLSListen := flag.String("proxy-tls-listen", env(proxyTLSListenEnv, defaultProxyTLSListen),
		"address the reverse proxy serves HTTPS on, used only with a domain in acme mode")
	certs := flag.String("certs", env(certsEnv, defaultCerts),
		"directory the certificates of acme mode are cached in")
	publicAddress := flag.String("public-address", env(publicAddressEnv, ""),
		"the address the proxy is reached on from outside, which a unit's declared hostname is proved against")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// A domain that is not a name, and a mode this daemon does not have, are
	// refused here rather than at the first request: a host told to serve a
	// domain and serving nothing is worse than one that did not start.
	if prob := checkProxySettings(*domain, *tlsMode, *publicAddress); prob != nil {
		return prob
	}

	// The kitbash-mcp of every MCP session is run as a member, so the path is
	// held to an absolute one here rather than resolved against whatever
	// working directory or PATH the daemon happens to have.
	if !filepath.IsAbs(*mcpBinary) {
		return fmt.Errorf("-mcp-binary %s is not an absolute path", *mcpBinary)
	}

	// A host upgraded from a kitbash that locked shadow's own /etc/subuid.lock
	// still carries the empty file that makes useradd refuse to run, so it is
	// cleared once here, see issue #83.
	clearLegacySubIDLock()

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
	// The values members set with secrets_set live beside the store and never
	// in it. The tree is prepared here rather than only in Prepare, and a
	// failure stops the daemon: a base directory kitbashd will not write into,
	// such as a symbolic link somebody put there, would otherwise show up as
	// every secrets call failing on a daemon that had reported itself started,
	// see internal/secrets.
	if err := secrets.New(*secretsDir).Prepare(); err != nil {
		return err
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
	// The proxy's listeners are bound here, as root, with the rest of them.
	// Nothing the proxy does afterwards needs root: it reads the routing
	// table, opens loopback connections to a Process and writes the store,
	// which is the daemon's own, see internal/daemon/proxy.go.
	var proxyLn, proxyTLSLn net.Listener
	if *domain != "" {
		proxyLn, err = net.Listen("tcp", *proxyListen)
		if err != nil {
			ln.Close()
			closeListener(otlpLn)
			return fmt.Errorf("listen on %s: %w", *proxyListen, err)
		}
		if *tlsMode == daemon.TLSACME {
			proxyTLSLn, err = net.Listen("tcp", *proxyTLSListen)
			if err != nil {
				ln.Close()
				closeListener(otlpLn)
				closeListener(proxyLn)
				return fmt.Errorf("listen on %s: %w", *proxyTLSListen, err)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := daemon.New(st, daemon.Options{
		Version:       version,
		MCPBinary:     *mcpBinary,
		NoRestore:     *noRestore,
		SecretsDir:    *secretsDir,
		Domain:        *domain,
		TLS:           *tlsMode,
		CertDir:       *certs,
		PublicAddress: *publicAddress,
	})
	defer srv.Close()
	if _, err := srv.Sweep(ctx); err != nil {
		// A store that cannot be swept can still receive and answer, so this
		// is a warning rather than a refusal to start.
		logger.Printf("retention sweep on start failed: %v", err)
	}
	go srv.SweepLoop(ctx, daemon.SweepEvery)
	// From here every internal cause is stored as a log record only an admin
	// reads, see PLAN.md section 2.4. Recording stops before the store is
	// closed, which is why this defer comes after the store's.
	stopRecording := srv.RecordInternalCauses()
	defer stopRecording()
	// One file is the whole store, so one copy of it is the whole backup,
	// see PLAN.md section 4.7.
	go srv.BackupLoop(ctx, "", 0)
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
	errs := make(chan error, 4)
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
	if proxyLn != nil {
		listeners++
		go func() {
			defer cancel()
			errs <- srv.ServeProxy(serve, proxyLn)
		}()
	}
	if proxyTLSLn != nil {
		listeners++
		go func() {
			defer cancel()
			errs <- srv.ServeProxyTLS(serve, proxyTLSLn)
		}()
	}
	if proxyLn != nil {
		logger.Printf("serving %s over %s, TLS %s", *domain, proxyAddresses(*proxyListen, proxyTLSLn, *proxyTLSListen), *tlsMode)
	}

	// Restore runs beside the listeners rather than before them: a host with
	// many containers would otherwise keep every member's session waiting on
	// podman. It logs one line when it is done, see PLAN.md section 2.3, and
	// it does nothing at all when the daemon was started with restore off.
	//
	// The cgroup tree comes first: every Process is started inside a cgroup of
	// its own under its owner's, so the tree exists before the first container
	// does. It also sweeps the environment files a daemon that stopped mid
	// start left behind, see internal/cgroups and internal/daemon/run.go.
	go func() {
		srv.Prepare(serve)
		srv.Restore(serve)
		// The ticker starts after restore as well, and for the same reason:
		// a job whose host is still bringing containers back would be started
		// while the runtime is busy with the boot. Ticks missed while the
		// daemon was down are not made up, so it starts from now, see
		// PLAN.md section 2.3.
		go srv.ScheduleLoop(serve)
		// The health probes start after restore rather than beside it: a
		// Process that is still coming back would be probed while its
		// container is starting, and the first record of the boot would say
		// unhealthy about a host that is merely booting, see PLAN.md
		// section 2.4.
		srv.HealthLoop(serve)
	}()

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

// clearLegacySubIDLock removes the subordinate id lock file an older kitbash
// left at shadow's own lock name. Only an empty file is removed, and a failure
// is a log line rather than a refusal to start: the daemon takes its own lock
// under /run and serves either way, while useradd on that host stays broken
// until an operator deletes the file.
func clearLegacySubIDLock() {
	removed, err := sysusers.RemoveLegacySubIDLock(sysusers.LegacySubIDLock)
	if err != nil {
		logger.Printf("could not remove the stale lock %s an older kitbash left behind: %v",
			sysusers.LegacySubIDLock, err)
		return
	}
	if removed {
		logger.Printf("removed the stale lock %s an older kitbash left behind; that name belongs to shadow-utils",
			sysusers.LegacySubIDLock)
	}
}

// checkProxySettings refuses a domain that is not a name and a TLS mode this
// daemon does not have. A host with no domain is not checked: that is a host
// that routes nothing, which is every kitbash host before the proxy existed.
func checkProxySettings(domain, mode, publicAddress string) error {
	if domain != "" && !manifest.ValidHostname(domain) {
		return fmt.Errorf("%s is %q, which is not a domain; it is a lower case DNS name of at least two labels, such as kitbash.example.org",
			domainEnv, domain)
	}
	if mode != daemon.TLSACME && mode != daemon.TLSGateway {
		return fmt.Errorf("%s is %q; it is %s, which obtains this host's own certificates, or %s, which trusts the gateway in front of it",
			tlsEnv, mode, daemon.TLSACME, daemon.TLSGateway)
	}
	if publicAddress != "" {
		if _, err := netip.ParseAddr(publicAddress); err != nil {
			return fmt.Errorf("%s is %q, which is not an address; it is the address the proxy is reached on from outside, such as 203.0.113.9",
				publicAddressEnv, publicAddress)
		}
	}
	return nil
}

// proxyAddresses is what the one start up line says about where the proxy is.
func proxyAddresses(http string, tls net.Listener, tlsAddress string) string {
	if tls == nil {
		return http
	}
	return http + " and " + tlsAddress
}

// closeListener closes one that was opened, for a start that fails after it.
func closeListener(ln net.Listener) {
	if ln != nil {
		ln.Close()
	}
}

// truthy reads a switch from the environment. Only 1 and true turn one on: a
// variable set to 0 or to false is an operator saying no, and reading any
// value as yes would leave a host without its Processes because somebody wrote
// KITBASH_NO_RESTORE=0.
func truthy(v string) bool {
	return v == "1" || strings.EqualFold(v, "true")
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
