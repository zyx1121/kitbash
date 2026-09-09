// Command kitbash-mcp serves the kitbash MCP surface over stdio. sshd runs it
// as the connecting Linux user through ForceCommand, so authentication and
// isolation are the operating system's job.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/server"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// version is set at build time with -X main.version.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kitbash-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" {
			fmt.Println(version)
			return nil
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	files, err := fs.NewFromEnv()
	if err != nil {
		return err
	}
	// Telemetry is opened before anything is served, so the first call is
	// traced. It connects nothing here: a host without kitbashd costs the
	// session one line in the server log and nothing else.
	telem := telemetry.NewFromEnv(version)
	defer func() {
		// The session is over by now, so the last flush is given a deadline
		// rather than the caller's patience.
		ctx, cancel := context.WithTimeout(context.Background(), telemetry.ShutdownTimeout)
		defer cancel()
		if err := telem.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "kitbash-mcp: flushing telemetry: %v\n", err)
		}
	}()

	// The same client is the approval queue: a member's write under /org is
	// queued with kitbashd instead of being attempted, see PLAN.md 2.1.
	files.SetApprovals(telem.Client())

	// What this session may call. A member's own session is narrowed by
	// nothing; the session of a Process is narrowed to what its Package
	// declared, and a declaration this build cannot read ends the session
	// here rather than serving the owner's whole surface, see PLAN.md 2.3.
	permits, err := server.PermitsFromEnv()
	if err != nil {
		return err
	}

	runner := podman.NewCLI()
	// The Process registry is the same client tel_query forwards through: a
	// Process is registered with kitbashd before its container starts, which
	// is what mints its Telemetry token, see PLAN.md section 2.4.
	processes := proc.New(files, runner, telem.Client())
	tools := bridge.New(files, processes, runner)
	defer tools.Close()
	srv := server.New(version, server.Deps{
		Files:     files,
		Packages:  pkg.New(files, runner, tools),
		Processes: processes,
		Bridge:    tools,
		Telemetry: telem,
		Permits:   permits,
	})
	// The tools of the caller's already running Processes join the surface
	// before the first request is served, see PLAN.md section 2.3.
	tools.Sync(ctx)

	// A closed stdin is how an SSH session ends, not a failure.
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
