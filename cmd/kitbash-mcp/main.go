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
	runner := podman.NewCLI()
	processes := proc.New(files, runner)
	tools := bridge.New(files, processes, runner)
	defer tools.Close()
	srv := server.New(version, server.Deps{
		Files:     files,
		Packages:  pkg.New(files, runner, tools),
		Processes: processes,
		Bridge:    tools,
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
