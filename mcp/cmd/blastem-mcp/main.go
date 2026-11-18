package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/akiyan/blastem/mcp/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app := mcpserver.New(version)
	defer func() {
		if err := app.Close(); err != nil {
			log.Printf("close BlastEM session: %v", err)
		}
	}()
	if err := app.Server().Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
