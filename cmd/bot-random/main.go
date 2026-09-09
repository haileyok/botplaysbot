// bot-random — the Go example bot CLI (spec §13).
//
// Env (or --arg equivalents):
//
//	APPVIEW_URL / --appview          appview base URL (default http://localhost:8080)
//	PDS_URL / --pds                  PDS base URL (required)
//	BOT_IDENTIFIER / --identifier    handle or DID (required)
//	BOT_APP_PASSWORD / --password    app password (required)
//	MAX_PLIES / --max-plies          ply cap per game (default 50; on cap the loop resigns)
//	SEEK_MAX_CONCURRENT / --seek-max-concurrent   standing seek maxConcurrent (default 3)
//	SEEK_GAME_TYPE / --seek-game-type            default bot.plays.bot.chess.move
//	SEEK_RATED / --seek-rated        "true"/"false" (default true)
//
// SIGTERM/SIGINT: stop the loop (cancel seek + best-effort resign of live
// games), then exit 0.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/haileyok/botplaysbot/internal/botclient"
)

func argOrEnv(args []string, name, env string) string {
	for i, a := range args {
		if a == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return os.Getenv(env)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	appviewURL := argOrEnv(os.Args[1:], "appview", "APPVIEW_URL")
	if appviewURL == "" {
		appviewURL = "http://localhost:8080"
	}
	pdsURL := argOrEnv(os.Args[1:], "pds", "PDS_URL")
	identifier := argOrEnv(os.Args[1:], "identifier", "BOT_IDENTIFIER")
	password := argOrEnv(os.Args[1:], "password", "BOT_APP_PASSWORD")
	maxPlies := int64(50)
	if v := argOrEnv(os.Args[1:], "max-plies", "MAX_PLIES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxPlies = n
		} else {
			logger.Error("invalid MAX_PLIES", "value", v)
			os.Exit(1)
		}
	}
	seekMaxConcurrent := int64(3)
	if v := argOrEnv(os.Args[1:], "seek-max-concurrent", "SEEK_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			seekMaxConcurrent = n
		}
	}
	seekGameType := argOrEnv(os.Args[1:], "seek-game-type", "SEEK_GAME_TYPE")
	if seekGameType == "" {
		seekGameType = "bot.plays.bot.chess.move"
	}
	rated := true
	if v := argOrEnv(os.Args[1:], "seek-rated", "SEEK_RATED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			rated = b
		}
	}

	if pdsURL == "" || identifier == "" || password == "" {
		logger.Error("bot-random: PDS_URL, BOT_IDENTIFIER, BOT_APP_PASSWORD required")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := botclient.New(appviewURL, botclient.SlogLogger{L: logger})
	session, err := client.Login(ctx, botclient.LoginOptions{
		PDSURL:     pdsURL,
		Identifier: identifier,
		Password:   password,
	})
	if err != nil {
		logger.Error("bot-random: login failed", "err", err)
		os.Exit(1)
	}
	logger.Info("bot-random logged in", "handle", session.Handle, "did", session.DID)

	loop := botclient.NewLoop(botclient.LoopOptions{
		Client: client,
		Author: botclient.RandomMoverAuthor,
		Seek: &botclient.SeekOptions{
			GameType:      seekGameType,
			Rated:         &rated,
			MaxConcurrent: seekMaxConcurrent,
		},
		PollMs:   time.Second,
		MaxPlies: maxPlies,
		Logger:   botclient.SlogLogger{L: logger},
	})
	if err := loop.Start(ctx); err != nil {
		logger.Error("bot-random: loop start failed", "err", err)
		os.Exit(1)
	}

	// Clean shutdown: stop loop (cancel seek → resign live games) → exit 0.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("bot-random: shutting down")
	loop.Stop(ctx)
	cancel()
	logger.Info("bot-random: stopped")
	os.Exit(0)
}
