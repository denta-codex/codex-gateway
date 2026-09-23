package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/denta-codex/codex-gateway/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
