// AC.14b — cross-language game: a Go random-mover (in-process BotLoop)
// against the TS random-mover (spawned as a subprocess via tsx), per spec
// §14 Phase H.
//
// Design: the Go test boots the in-process appview + PDS harness + isolated
// DB (same helpers as every appview integration test), creates two agent
// accounts, runs the Go bot in-process as one, and spawns
// `node_modules/.bin/tsx src/random-mover-main.ts` for the other with the
// documented env contract (PLAYSBOT_APPVIEW_URL / PLAYSBOT_PDS_URL /
// PLAYSBOT_IDENTIFIER / PLAYSBOT_PASSWORD / MAX_PLIES).
//
// Assertions (post-game integrity):
//   - the game finishes (win or draw);
//   - both DIDs have repo move records with nonempty moveTokens;
//   - ≥1 delayed AND ≥1 sealed commentary across both repos;
//   - end-of-game key publication records (public commentary, 32-byte key);
//   - /api/game reveals decrypted texts post-game;
//   - a game.reveal record with agentPublished keys for published keys.
//
// The test is unconditional: it skips ONLY when the environment genuinely
// cannot run it (no Postgres, no node, no tsx binary — CI has all three).
package appview_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/haileyok/botplaysbot/internal/botclient"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/testutil"
)

// tsxBinary resolves the tsx devDependency binary, reporting whether the
// cross-language test can run at all.
func tsxBinary(t *testing.T) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("cross-language test: node not found in PATH; cannot spawn the TS random-mover (CI has node)")
		return "", ""
	}
	// Walk up from the test's package dir to the repo root (go.mod).
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("cross-language test: cannot locate repository root")
			return "", ""
		}
		dir = parent
	}
	tsx := filepath.Join(dir, "packages", "bots", "node_modules", ".bin", "tsx")
	if _, err := os.Stat(tsx); err != nil {
		tsxRoot := filepath.Join(dir, "node_modules", ".bin", "tsx")
		if _, err := os.Stat(tsxRoot); err != nil {
			t.Skip("cross-language test: tsx not installed (run: pnpm install); CI runs pnpm install first")
			return "", ""
		}
		tsx = tsxRoot
	}
	return node, tsx
}

// tsAgent is a running TS random-mover subprocess.
type tsAgent struct {
	cmd *exec.Cmd
	log func() string

	stopOnce sync.Once
}

// stop terminates the subprocess (SIGTERM then SIGKILL after a grace).
func (a *tsAgent) stop() {
	a.stopOnce.Do(func() {
		if a.cmd == nil || a.cmd.Process == nil {
			return
		}
		_ = a.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = a.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = a.cmd.Process.Kill()
			<-done
		}
	})
}

// startTSAgent spawns the TS random-mover-main.ts with the documented env
// contract. tsx resolves to either the pnpm .bin shim (a shell script —
// exec it directly) or a node entry; the shim is the documented
// `node_modules/.bin/tsx src/random-mover-main.ts` invocation. Its combined
// output is captured in a rolling buffer for failure diagnostics.
func startTSAgent(t *testing.T, node, tsx, script string, env []string) *tsAgent {
	t.Helper()
	cmd := exec.Command(tsx, script)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout // merge: both pipes are cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start TS random-mover: %v", err)
	}

	var mu sync.Mutex
	var buf strings.Builder
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			mu.Lock()
			if buf.Len() > 16_000 {
				buf.Reset()
			}
			buf.WriteString(sc.Text() + "\n")
			mu.Unlock()
		}
	}()
	agent := &tsAgent{cmd: cmd, log: func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}}
	t.Cleanup(agent.stop)
	return agent
}

// listRecordsFor lists one account's records of a collection from its PDS.
func listRecordsFor(t *testing.T, env *gameEnv, a testutil.Account, collection string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	var out []map[string]any
	cursor := ""
	for {
		res, err := comatprotoListRecords(ctx, env, a, collection, cursor)
		if err != nil {
			t.Fatalf("listRecords(%s) for %s: %v", collection, a.DID, err)
		}
		out = append(out, res.records...)
		if res.cursor == "" {
			return out
		}
		cursor = res.cursor
	}
}

type listOut struct {
	records []map[string]any
	cursor  string
}

// comatprotoListRecords lists records via the harness PDS with the account's
// session (raw HTTP: the records are decoded generically for assertions).
func comatprotoListRecords(ctx context.Context, env *gameEnv, a testutil.Account, collection, cursor string) (listOut, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	u := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=%s&limit=100", env.cfg.PDSURL, a.DID, collection)
	if cursor != "" {
		u += "&cursor=" + cursor
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return listOut{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return listOut{}, err
	}
	defer res.Body.Close()
	var parsed struct {
		Records []struct {
			URI   string         `json:"uri"`
			CID   string         `json:"cid"`
			Value map[string]any `json:"value"`
		} `json:"records"`
		Cursor string `json:"cursor"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return listOut{}, err
	}
	out := listOut{cursor: parsed.Cursor}
	for _, r := range parsed.Records {
		out.records = append(out.records, r.Value)
	}
	return out, nil
}

// TestAC14bCrossLanguageGame runs the full cross-language scenario: the Go
// bot (in-process BotLoop) vs the TS random-mover (tsx subprocess).
func TestAC14bCrossLanguageGame(t *testing.T) {
	node, tsx := tsxBinary(t)
	ctx := context.Background()

	// Boot: isolated DB + PDS harness + in-process appview with real service
	// account, fast pairing/sweeps (same shape as bootMatchEnv + fast
	// sweeper, since the reveal scheduler runs on the sweep).
	databaseURL := testutil.IsolatedDBURL(t)
	harness := testutil.StartPDS(t)

	svc := harness.CreateAccount(t)
	cfg := testConfig(t, databaseURL, t.TempDir(), harness.PDS, harness.PLC, svc.DID)
	cfg.ServiceAppPassword = svc.AppPassword
	cfg.Tunables.SweeperInterval = 250 * time.Millisecond
	cfg.Tunables.PairingInterval = 50 * time.Millisecond
	cfg.Tunables.MatchGrace = time.Second

	env := &gameEnv{testEnv: bootApp(t, cfg), svc: svc, harness: harness}
	white := harness.CreateAccount(t)
	black := harness.CreateAccount(t)

	// --- Go random-mover, in-process, as white. ---------------------------
	goLoopLog := newLoopLogger()
	goClient := botclient.New(env.server.URL, goLoopLog)
	if _, err := goClient.Login(ctx, botclient.LoginOptions{
		PDSURL:     harness.PDS,
		Identifier: white.Handle,
		Password:   white.AppPassword,
	}); err != nil {
		t.Fatalf("Go bot login: %v", err)
	}

	// Note: Seek with Rated=nil defaults to RATED server-side (gt.Option zero
	// → MatchSeek_Input.Rated is unset → p.Rated=false on the server). Both
	// bots must agree on the pool, so default to rated=true explicitly.
	rated := true
	goLoop := botclient.NewLoop(botclient.LoopOptions{
		Client:   goClient,
		Author:   botclient.RandomMoverAuthor,
		Seek:     &botclient.SeekOptions{GameType: engine.NSIDChessMove, Rated: &rated, MaxConcurrent: 3},
		PollMs:   300 * time.Millisecond,
		MaxPlies: 50,
		Logger:   goLoopLog,
	})
	if err := goLoop.Start(ctx); err != nil {
		t.Fatalf("Go bot loop start: %v", err)
	}
	t.Cleanup(func() { goLoop.Stop(context.Background()) })

	// --- TS random-mover subprocess, as black. ----------------------------
	root := repoRootDir(t)
	tsAgent := startTSAgent(t, node, tsx, filepath.Join(root, "packages", "bots", "src", "random-mover-main.ts"), []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PLAYSBOT_APPVIEW_URL=" + env.server.URL,
		"PLAYSBOT_PDS_URL=" + harness.PDS,
		"PLAYSBOT_IDENTIFIER=" + black.Handle,
		"PLAYSBOT_PASSWORD=" + black.AppPassword,
		"MAX_PLIES=50",
	})
	t.Cleanup(tsAgent.stop)

	// --- Wait for pairing. ------------------------------------------------
	t.Cleanup(func() {
		t.Logf("go bot log:\n%s", goLoopLog.String())
		t.Logf("ts bot log:\n%s", tsAgent.log())
	})
	gameURI := waitForString(t, "a matched game", 120*time.Second, func() string {
		return goLoop.ActiveGame()
	})
	t.Logf("game matched: %s", gameURI)

	// Pin the match: withdraw the Go side's standing seek so no cascade
	// games can pair while the test watches this one to completion.
	goLoop.CancelSeek(ctx)

	// --- Wait for the game to finish. ------------------------------------
	finished := func() (string, bool) {
		api, err := goClient.APIGame(ctx, gameURI)
		if err != nil {
			return "", false
		}
		status, _ := api["status"].(string)
		if status == "finished" || status == "aborted" {
			return status, true
		}
		return status, false
	}
	status := ""
	waitFor(t, "game finish", 300*time.Second, func() bool {
		s, ok := finished()
		status = s
		return ok
	})
	if status != "finished" {
		t.Fatalf("game status = %s, want finished (go bot log:\n%s\nts bot log:\n%s)",
			status, goLoopLog.String(), tsAgent.log())
	}

	// The standing seek keeps the loop pairing new games by design (§9a.3);
	// stop it so the session cleanup (key publication already happened at
	// finish) runs and integrity can assert a stable record set.
	goLoop.Stop(context.Background())
	waitFor(t, "Go loop idle after stop", 30*time.Second, func() bool {
		return goLoop.Idle()
	})

	assertCrossLanguageIntegrity(t, env, gameURI, white, black, tsAgent, goLoopLog)
}

// assertCrossLanguageIntegrity pins the post-game record integrity (the Go
// mirror of the TS harness's assertGameIntegrity).
func assertCrossLanguageIntegrity(t *testing.T, env *gameEnv, gameURI string, white, black testutil.Account, tsAgent *tsAgent, goLoopLog *testLoopLogger) {
	accounts := map[string]testutil.Account{white.DID: white, black.DID: black}

	waitFor(t, "post-game record integrity", 60*time.Second, func() bool {
		status, body := env.get(t, "/api/game?uri="+gameURIEscape(gameURI), nil)
		if status != http.StatusOK {
			return false
		}
		var game struct {
			Ply     int64 `json:"ply"`
			History []struct {
				Ply    int64  `json:"ply"`
				Player string `json:"player"`
			} `json:"history"`
			Commentary []struct {
				Visibility string  `json:"visibility"`
				Revealed   bool    `json:"revealed"`
				Text       *string `json:"text"`
				Player     string  `json:"player"`
			} `json:"commentary"`
			Reveal *struct {
				Keys []struct {
					KeyID          string `json:"keyId"`
					AgentPublished bool   `json:"agentPublished"`
				} `json:"keys"`
			} `json:"reveal"`
		}
		if err := json.Unmarshal(body, &game); err != nil {
			return false
		}

		// Every accepted move has a repo move record with a nonempty
		// moveToken, for BOTH the Go-written (atmos types) and the
		// TS-written (@atproto/api) repos.
		for did, account := range accounts {
			records := listRecordsFor(t, env, account, playsbot.NSIDGameMove)
			var forGame []map[string]any
			for _, r := range records {
				if g, ok := r["game"].(map[string]any); ok && g["uri"] == gameURI {
					forGame = append(forGame, r)
				}
			}
			var mine []int64
			for _, h := range game.History {
				if h.Player == did {
					mine = append(mine, h.Ply)
				}
			}
			if len(forGame) < len(mine) {
				return false
			}
			recPlies := map[int64]bool{}
			for _, rec := range forGame {
				token, _ := rec["moveToken"].(string)
				if token == "" {
					t.Fatalf("%s: move record missing moveToken: %+v", did, rec)
				}
				plyF, _ := rec["ply"].(float64)
				recPlies[int64(plyF)] = true
			}
			for _, ply := range mine {
				if !recPlies[ply] {
					return false // record not indexed yet; keep polling
				}
			}
		}

		// ≥1 delayed and ≥1 sealed commentary across both repos.
		var visibilities = map[string]bool{}
		var keyPubs []map[string]any
		for _, account := range accounts {
			recs := listRecordsFor(t, env, account, playsbot.NSIDGameCommentary)
			for _, r := range recs {
				g, ok := r["game"].(map[string]any)
				if !ok || g["uri"] != gameURI {
					continue
				}
				vis, _ := r["visibility"].(string)
				visibilities[vis] = true
				if vis == "public" {
					if keyID, _ := r["keyId"].(string); keyID != "" {
						if text, _ := r["text"].(string); text != "" {
							keyPubs = append(keyPubs, r)
						}
					}
				}
			}
		}
		if !visibilities["delayed"] || !visibilities["sealed"] {
			return false
		}
		if len(keyPubs) < 1 {
			return false
		}
		for _, kp := range keyPubs {
			text, _ := kp["text"].(string)
			raw, err := base64.StdEncoding.DecodeString(text)
			if err != nil || len(raw) != 32 {
				t.Fatalf("published key not 32 bytes: %q (err %v)", text, err)
			}
		}

		// /api/game reveals decrypted texts post-game.
		revealed := 0
		for _, c := range game.Commentary {
			if c.Visibility != "public" && c.Revealed {
				if c.Text == nil || *c.Text == "" {
					t.Fatalf("revealed commentary without text: %+v", c)
				}
				revealed++
			}
		}
		if revealed < 1 {
			return false
		}

		return true
	})

	// game.reveal record with agentPublished keys (service repo).
	var rec *playsbot.GameReveal
	waitFor(t, "reveal record", 30*time.Second, func() bool {
		for _, r := range revealRecords(t, env) {
			if r.Game.URI == gameURI {
				rec = &r
				return true
			}
		}
		return false
	})
	// The reveal RECORD is frozen at game end — agentPublished there reflects
	// what was known at write time (agents publish keys AFTER #gameFinished,
	// so false is correct-at-write). The live rows update when the
	// publication records arrive; assert ≥1 agentPublished key via the site
	// summary (what spectators verify against).
	if len(rec.Keys) < 2 {
		t.Fatalf("reveal record has %d keys, want both players': %+v", len(rec.Keys), rec.Keys)
	}
	waitFor(t, "agentPublished key in reveal summary", 60*time.Second, func() bool {
		u := env.server.URL + "/api/game?uri=" + url.QueryEscape(gameURI)
		res, err := http.Get(u)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		var api map[string]any
		if err := json.NewDecoder(res.Body).Decode(&api); err != nil {
			return false
		}
		reveal, _ := api["reveal"].(map[string]any)
		if reveal == nil {
			return false
		}
		keys, _ := reveal["keys"].([]any)
		for _, k := range keys {
			if m, ok := k.(map[string]any); ok {
				if v, _ := m["agentPublished"].(bool); v {
					return true
				}
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// plumbing

// repoRootDir walks up to the go.mod root.
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}

// waitFor polls until cond returns true, failing the test with context.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForString polls until cond returns a nonempty string.
func waitForString(t *testing.T, what string, timeout time.Duration, cond func() string) string {
	t.Helper()
	var got string
	waitFor(t, what, timeout, func() bool {
		got = cond()
		return got != ""
	})
	return got
}

// testLoopLogger buffers bot loop chatter and dumps it on test failure
// (t.Logf is not safe to call from loop goroutines after the test ends).
type testLoopLogger struct {
	mu  sync.Mutex
	buf strings.Builder
}

func newLoopLogger() *testLoopLogger { return &testLoopLogger{} }

func (l *testLoopLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() > 16_000 {
		l.buf.Reset()
	}
	fmt.Fprintf(&l.buf, format, args...)
	l.buf.WriteString("\n")
}

func (l *testLoopLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
