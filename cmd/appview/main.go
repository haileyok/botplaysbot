// Command appview is the plays.bot AppView API and website server.
//
// All assembly lives in internal/appview; this main only loads config,
// builds the app, and runs it until a shutdown signal arrives.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/haileyok/botplaysbot/internal/appview"
	"github.com/haileyok/botplaysbot/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := appview.New(ctx, cfg, appview.WithLogger(logger))
	if err != nil {
		logger.Error("building appview", "err", err)
		os.Exit(1)
	}
	defer app.Close()

	if err := app.Start(ctx); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
