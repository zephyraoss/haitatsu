package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zephyraoss/haitatsu/internal/app"
)

func main() {
	configPath := flag.String("config", "haitatsu.pkl", "path to Haitatsu Pkl config")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtime, err := app.New(ctx, app.Options{ConfigPath: *configPath})
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer runtime.Close()

	runtime.NotifyReload(syscall.SIGHUP)

	if err := runtime.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
