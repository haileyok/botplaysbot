package config

import (
	"testing"
	"time"
)

func baseEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://playsbot:playsbot@localhost:5432/playsbot?sslmode=disable",
	}
}

func getFrom(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestDefaults(t *testing.T) {
	cfg, err := LoadFromEnv(getFrom(baseEnv()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.DatabaseURL != baseEnv()["DATABASE_URL"] {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.EventSource != EventSourceFirehose {
		t.Errorf("EventSource = %q, want firehose", cfg.EventSource)
	}
	if cfg.EventSourceURL != DefaultEventSourceURL[EventSourceFirehose] {
		t.Errorf("EventSourceURL = %q", cfg.EventSourceURL)
	}

	want := Tunables{
		PerMoveSeconds:      300,
		PairingInterval:     2 * time.Second,
		RatingWindow:        200,
		WindowWiden:         50,
		WindowWidenInterval: 30 * time.Second,
		WindowCap:           800,
		RepeatCooldown:      10 * time.Minute,
		MatchGrace:          10 * time.Second,
		StandingExpire:      15 * time.Minute,
		MissingRecordWindow: 10 * time.Minute,
		CommentaryDelay:     CommentaryDelay{Plies: 2, Seconds: 300},
		MaxConcurrentGames:  20,
		SweeperInterval:     time.Second,
		ChallengeTTL:        10 * time.Minute,
		ChallengeMaxTTL:     24 * time.Hour,
		NoShowSuspend:       time.Hour,
		DistinctOperators:   true,
	}
	if cfg.Tunables != want {
		t.Errorf("Tunables = %+v, want %+v", cfg.Tunables, want)
	}
}

func TestOverrides(t *testing.T) {
	env := baseEnv()
	env["PLAYSBOT_PORT"] = "9090"
	env["PLAYSBOT_EVENT_SOURCE"] = "JETSTREAM"
	env["PLAYSBOT_PDS_URL"] = "https://pds.example.com/"
	env["PLAYSBOT_SERVICE_DID"] = "did:web:bot.plays.bot"
	env["PLAYSBOT_EVENT_SOURCE_URL"] = "wss://jetstream.example.com/subscribe"
	env["PLAYSBOT_PER_MOVE_SECONDS"] = "60"
	env["PLAYSBOT_PAIRING_INTERVAL"] = "5s"
	env["PLAYSBOT_REPEAT_COOLDOWN"] = "30m"
	env["PLAYSBOT_COMMENTARY_DELAY_PLIES"] = "4"
	env["PLAYSBOT_MAX_CONCURRENT_GAMES"] = "3"
	env["PLAYSBOT_CHALLENGE_TTL"] = "5m"
	env["PLAYSBOT_NO_SHOW_SUSPEND"] = "30m"
	env["PLAYSBOT_DISTINCT_OPERATORS"] = "false"

	cfg, err := LoadFromEnv(getFrom(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Port != 9090 {
		t.Errorf("Port = %d", cfg.Port)
	}
	if cfg.EventSource != EventSourceJetstream {
		t.Errorf("EventSource = %q", cfg.EventSource)
	}
	if cfg.EventSourceURL != "wss://jetstream.example.com/subscribe" {
		t.Errorf("EventSourceURL = %q", cfg.EventSourceURL)
	}
	if cfg.PDSURL != "https://pds.example.com" {
		t.Errorf("PDSURL trailing slash not trimmed: %q", cfg.PDSURL)
	}
	if cfg.ServiceDID != "did:web:bot.plays.bot" {
		t.Errorf("ServiceDID = %q", cfg.ServiceDID)
	}
	if cfg.Tunables.PerMoveSeconds != 60 ||
		cfg.Tunables.PairingInterval != 5*time.Second ||
		cfg.Tunables.RepeatCooldown != 30*time.Minute ||
		cfg.Tunables.CommentaryDelay.Plies != 4 ||
		cfg.Tunables.MaxConcurrentGames != 3 ||
		cfg.Tunables.ChallengeTTL != 5*time.Minute ||
		cfg.Tunables.NoShowSuspend != 30*time.Minute ||
		cfg.Tunables.DistinctOperators {
		t.Errorf("Tunables = %+v", cfg.Tunables)
	}
}

func TestJetstreamDefaultURL(t *testing.T) {
	env := baseEnv()
	env["PLAYSBOT_EVENT_SOURCE"] = "jetstream"
	cfg, err := LoadFromEnv(getFrom(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.EventSourceURL != DefaultEventSourceURL[EventSourceJetstream] {
		t.Errorf("EventSourceURL = %q", cfg.EventSourceURL)
	}
}

func TestMissingDatabaseURLFails(t *testing.T) {
	if _, err := LoadFromEnv(getFrom(map[string]string{})); err == nil {
		t.Fatal("expected error for missing PLAYSBOT_DATABASE_URL")
	}
}

func TestInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"bad event source", map[string]string{"PLAYSBOT_EVENT_SOURCE": "carrier-pigeon"}},
		{"port out of range", map[string]string{"PLAYSBOT_PORT": "70000"}},
		{"port not numeric", map[string]string{"PLAYSBOT_PORT": "http"}},
		{"zero per-move", map[string]string{"PLAYSBOT_PER_MOVE_SECONDS": "0"}},
		{"negative window cap", map[string]string{"PLAYSBOT_WINDOW_CAP": "-1"}},
		{"zero max concurrent", map[string]string{"PLAYSBOT_MAX_CONCURRENT_GAMES": "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range tc.env {
				env[k] = v
			}
			if _, err := LoadFromEnv(getFrom(env)); err == nil {
				t.Fatalf("expected error, got nil (env %v)", tc.env)
			}
		})
	}
}
