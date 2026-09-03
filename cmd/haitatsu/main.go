package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zephyraoss/haitatsu/internal/app"
	"github.com/zephyraoss/haitatsu/internal/version"
)

const defaultConfigPath = "/etc/haitatsu/haitatsu.pkl"

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to Haitatsu Pkl config")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	if err := requireConfigFile(*configPath); err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}

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

func requireConfigFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config file %s does not exist: mount a Pkl config there or pass -config", path)
	}
	if err != nil {
		return fmt.Errorf("stat config file %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("config path %s is a directory: mount a Pkl config file there or pass -config", path)
	}
	return nil
}
