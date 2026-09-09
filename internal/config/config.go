// Package config loads plays.bot AppView configuration from the environment.
//
// The loader is pure with respect to the process: pass an env map (or use
// LoadFromEnv with a getter) in tests. See .env.example at the repository root
// for the full, documented variable list.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// EventSource selects how the indexer consumes ATProto repo events.
type EventSource string

const (
	EventSourceFirehose  EventSource = "firehose"
	EventSourceJetstream EventSource = "jetstream"
)

// Valid reports whether s is a known event source.
func (e EventSource) Valid() bool {
	switch e {
	case EventSourceFirehose, EventSourceJetstream:
		return true
	default:
		return false
	}
}

// DefaultEventSourceURL is used when EVENT_SOURCE_URL is unset, keyed by source.
var DefaultEventSourceURL = map[EventSource]string{
	EventSourceFirehose:  "wss://bsky.network",
	EventSourceJetstream: "wss://jetstream1.us-east.bsky.network/subscribe",
}

// CommentaryDelay controls the broadcast delay for delayed commentary
// (spec §8.2): reveal at whichever comes first of plies or seconds.
type CommentaryDelay struct {
	Plies   int `json:"plies"`
	Seconds int `json:"seconds"`
}

// Tunables are the operational knobs with spec-mandated defaults.
type Tunables struct {
	// PerMoveSeconds is the default per-move clock budget (spec §7).
	PerMoveSeconds int
	// PairingInterval is how often the matchmaker pairing loop runs (§9a.3).
	PairingInterval time.Duration
	// RatingWindow is the initial acceptable rating difference for pairing (§9a.3).
	RatingWindow int
	// WindowWiden is how much the rating window widens per WindowWidenInterval.
	WindowWiden int
	// WindowWidenInterval is the widening period (50 per 30s by default).
	WindowWidenInterval time.Duration
	// WindowCap is the maximum effective rating window.
	WindowCap int
	// RepeatCooldown is the minimum gap between games for the same pair (§9a.3).
	RepeatCooldown time.Duration
	// MatchGrace is the delay between #matched notification and clock start (§9a.3).
	MatchGrace time.Duration
	// StandingExpire is how long a standing seek survives without a live
	// match.subscribe connection (§9a.3).
	StandingExpire time.Duration
	// MissingRecordWindow is how long the indexer waits for an accepted move's
	// repo record before flagging missingMoveRecord (§10).
	MissingRecordWindow time.Duration
	// CommentaryDelay is the broadcast delay {plies, seconds} (§8.2).
	CommentaryDelay CommentaryDelay
	// MaxConcurrentGames caps active games per DID (§10: 20, configurable).
	MaxConcurrentGames int
}

// Config is the process configuration.
type Config struct {
	// DatabaseURL is the Postgres connection string. Required.
	DatabaseURL string
	// PDSURL is the PDS the AppView service account writes through.
	PDSURL string
	// ServiceDID is the AppView service DID (owner of verdict records).
	ServiceDID string
	// ServiceAppPassword authenticates the service account to its PDS (v1).
	ServiceAppPassword string
	// Port is the HTTP listen port.
	Port int
	// EventSource selects firehose or jetstream ingestion.
	EventSource EventSource
	// EventSourceURL is the websocket endpoint for the event source.
	EventSourceURL string

	Tunables Tunables
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	return LoadFromEnv(os.Getenv)
}

// LoadFromEnv reads configuration through the given getter. Missing optional
// variables take documented defaults; DATABASE_URL is required.
func LoadFromEnv(get func(string) string) (*Config, error) {
	cfg := &Config{}

	cfg.DatabaseURL = get("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}

	cfg.PDSURL = strings.TrimRight(get("PLAYSBOT_PDS_URL"), "/")
	cfg.ServiceDID = get("PLAYSBOT_SERVICE_DID")
	cfg.ServiceAppPassword = get("PLAYSBOT_SERVICE_APP_PASSWORD")

	port, err := intEnv(get, "PLAYSBOT_PORT", 8080)
	if err != nil {
		return nil, err
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("config: PLAYSBOT_PORT %d out of range", port)
	}
	cfg.Port = port

	cfg.EventSource = EventSource(strings.ToLower(get("PLAYSBOT_EVENT_SOURCE")))
	if cfg.EventSource == "" {
		cfg.EventSource = EventSourceFirehose
	}
	if !cfg.EventSource.Valid() {
		return nil, fmt.Errorf("config: PLAYSBOT_EVENT_SOURCE must be firehose or jetstream, got %q", cfg.EventSource)
	}
	cfg.EventSourceURL = strings.TrimRight(get("PLAYSBOT_EVENT_SOURCE_URL"), "/")
	if cfg.EventSourceURL == "" {
		cfg.EventSourceURL = DefaultEventSourceURL[cfg.EventSource]
	}

	perMoveSeconds, err := intEnv(get, "PLAYSBOT_PER_MOVE_SECONDS", 300)
	if err != nil {
		return nil, err
	}
	pairingInterval, err := durEnv(get, "PLAYSBOT_PAIRING_INTERVAL", 2*time.Second)
	if err != nil {
		return nil, err
	}
	ratingWindow, err := intEnv(get, "PLAYSBOT_RATING_WINDOW", 200)
	if err != nil {
		return nil, err
	}
	windowWiden, err := intEnv(get, "PLAYSBOT_WINDOW_WIDEN", 50)
	if err != nil {
		return nil, err
	}
	windowWidenInterval, err := durEnv(get, "PLAYSBOT_WINDOW_WIDEN_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	windowCap, err := intEnv(get, "PLAYSBOT_WINDOW_CAP", 800)
	if err != nil {
		return nil, err
	}
	repeatCooldown, err := durEnv(get, "PLAYSBOT_REPEAT_COOLDOWN", 10*time.Minute)
	if err != nil {
		return nil, err
	}
	matchGrace, err := durEnv(get, "PLAYSBOT_MATCH_GRACE", 10*time.Second)
	if err != nil {
		return nil, err
	}
	standingExpire, err := durEnv(get, "PLAYSBOT_STANDING_EXPIRE", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	missingRecordWindow, err := durEnv(get, "PLAYSBOT_MISSING_RECORD_WINDOW", 10*time.Minute)
	if err != nil {
		return nil, err
	}
	commentaryDelayPlies, err := intEnv(get, "PLAYSBOT_COMMENTARY_DELAY_PLIES", 2)
	if err != nil {
		return nil, err
	}
	commentaryDelaySeconds, err := intEnv(get, "PLAYSBOT_COMMENTARY_DELAY_SECONDS", 300)
	if err != nil {
		return nil, err
	}
	maxConcurrentGames, err := intEnv(get, "PLAYSBOT_MAX_CONCURRENT_GAMES", 20)
	if err != nil {
		return nil, err
	}

	t := Tunables{
		PerMoveSeconds:      perMoveSeconds,
		PairingInterval:     pairingInterval,
		RatingWindow:        ratingWindow,
		WindowWiden:         windowWiden,
		WindowWidenInterval: windowWidenInterval,
		WindowCap:           windowCap,
		RepeatCooldown:      repeatCooldown,
		MatchGrace:          matchGrace,
		StandingExpire:      standingExpire,
		MissingRecordWindow: missingRecordWindow,
		CommentaryDelay: CommentaryDelay{
			Plies:   commentaryDelayPlies,
			Seconds: commentaryDelaySeconds,
		},
		MaxConcurrentGames: maxConcurrentGames,
	}
	if t.PerMoveSeconds <= 0 {
		return nil, fmt.Errorf("config: PLAYSBOT_PER_MOVE_SECONDS must be > 0")
	}
	if t.PairingInterval <= 0 || t.WindowWidenInterval <= 0 || t.RepeatCooldown < 0 ||
		t.MatchGrace < 0 || t.StandingExpire <= 0 || t.MissingRecordWindow <= 0 {
		return nil, fmt.Errorf("config: invalid interval tunable")
	}
	if t.RatingWindow < 0 || t.WindowWiden < 0 || t.WindowCap < t.RatingWindow {
		return nil, fmt.Errorf("config: invalid rating window tunables")
	}
	if t.CommentaryDelay.Plies < 0 || t.CommentaryDelay.Seconds < 0 {
		return nil, fmt.Errorf("config: commentary delay tunables must be >= 0")
	}
	if t.MaxConcurrentGames <= 0 {
		return nil, fmt.Errorf("config: PLAYSBOT_MAX_CONCURRENT_GAMES must be > 0")
	}
	cfg.Tunables = t

	return cfg, nil
}

// intEnv parses an optional integer variable. Empty uses def; a malformed
// value is an error rather than a silent fallback.
func intEnv(get func(string) string, name string, def int) (int, error) {
	s := get(name)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", name, err)
	}
	return v, nil
}

// durEnv parses an optional duration variable. Empty uses def; a malformed
// value is an error rather than a silent fallback.
func durEnv(get func(string) string, name string, def time.Duration) (time.Duration, error) {
	s := get(name)
	if s == "" {
		return def, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", name, err)
	}
	return v, nil
}
