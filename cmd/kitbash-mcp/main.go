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

	"github.com/zyx1121/kitbash/internal/fs"
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
	// A closed stdin is how an SSH session ends, not a failure.
	if err := server.New(version, files).Run(ctx, &mcp.StdioTransport{}); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
