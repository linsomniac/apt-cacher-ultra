// Command apt-cacher-ultra is a robust apt repository cache. See SPEC.md.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	configPath := flag.String("config", "/etc/apt-cacher-ultra/config.toml", "path to TOML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}

	if err := run(*configPath); err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.Defaults()

	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	slog.Info("apt-cacher-ultra starting",
		"version", Version,
		"config_path", configPath,
		"listen", cfg.Cache.Listen,
		"cache_dir", cfg.Cache.Dir,
	)

	// Server wiring lands in subsequent commits. This scaffolding stage
	// validates config-load + logging and exits cleanly.
	slog.Info("scaffolding stage; server not yet implemented")
	return nil
}

func newLogger(c config.LogConfig) *slog.Logger {
	var level slog.Level
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
