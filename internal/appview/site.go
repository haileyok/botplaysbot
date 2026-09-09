// Site JSON APIs (Phase G): plain JSON endpoints backing the embedded
// website. These are NOT lexicon-bound — they compose existing repos and
// the game manager into shapes the React pages consume, with site-only
// enrichments the XRPC lexicons do not carry (per-commentary receipt
// status, the post-game reveal summary for the verify affordance).
//
// Endpoints (all same-origin, no CORS):
//
//	GET /api/games                active games grid
//	GET /api/game?uri=<at-uri>    full getState payload + enrichments
//	GET /api/actors/{did}         actor page data
//	GET /api/challenges?open=true open-challenge lobby strip
//	GET /docs and /docs/assets/*  embedded lexicon markdown + policies page
package appview

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// ---------------------------------------------------------------------------
// shared rendering helpers

// seatLabel renders the seat a DID holds in one game, "" when absent.
func seatLabel(players []games.GamePlayer, did string) string {
	for _, p := range players {
		if p.DID == did {
			return p.Seat
		}
	}
	return ""
}

// isoPtr formats a nullable time as ISO-8601 ("" when nil).
func isoPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// strDeref is a nil-safe string pointer accessor.
func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------------------------------------------------------------------------
// GET /api/games

type sitePlayer struct {
	DID             string  `json:"did"`
	Seat            string  `json:"seat"`
	ProfileRevision *int64  `json:"profileRevision,omitempty"`
	ProfileHash     *string `json:"profileHash,omitempty"`
}

type siteGame struct {
	URI        string       `json:"uri"`
	GameType   string       `json:"gameType"`
	Variant    *string      `json:"variant,omitempty"`
	Status     string       `json:"status"`
	Ply        int64        `json:"ply"`
	Turn       string       `json:"turn,omitempty"`
	Players    []sitePlayer `json:"players"`
	Clocks     []siteClock  `json:"clocks"`
	Position   any          `json:"position,omitempty"`
	Result     *siteResult  `json:"result,omitempty"`
	CreatedAt  string       `json:"createdAt,omitempty"`
	StartedAt  string       `json:"startedAt,omitempty"`
	FinishedAt string       `json:"finishedAt,omitempty"`
}

func siteGameOf(v *games.GameView) *siteGame {
	g := &siteGame{
		URI:        v.URI,
		GameType:   v.GameType,
		Variant:    v.Variant,
		Status:     v.Status,
		Ply:        v.Ply,
		Turn:       v.TurnDID,
		Players:    make([]sitePlayer, 0, len(v.Players)),
		Clocks:     []siteClock{},
		CreatedAt:  isoPtr(v.CreatedAt),
		StartedAt:  isoPtr(v.StartedAt),
		FinishedAt: isoPtr(v.FinishedAt),
	}
	for _, p := range v.Players {
		g.Players = append(g.Players, sitePlayer{
			DID:             p.DID,
			Seat:            p.Seat,
			ProfileRevision: p.ProfileRevision,
			ProfileHash:     p.ProfileHash,
		})
	}
	for _, c := range v.Clocks {
		g.Clocks = append(g.Clocks, siteClock{
			DID:         c.DID,
			RemainingMs: c.RemainingMs,
			Deadline:    c.Deadline,
		})
	}
	if pos, ok := games.RenderPosition(v.Position); ok {
		g.Position = pos
	}
	if v.Result != nil {
		r := &siteResult{Outcome: v.Result.Outcome, Reason: v.Result.Reason}
		if v.Result.Winner != "" {
			w := v.Result.Winner
			r.Winner = &w
		}
		g.Result = r
	}
	return g
}

type siteClock struct {
	DID         string `json:"did"`
	RemainingMs int64  `json:"remainingMs"`
	Deadline    string `json:"deadline,omitempty"`
}

type siteResult struct {
	Outcome string  `json:"outcome"`
	Reason  string  `json:"reason"`
	Winner  *string `json:"winner,omitempty"`
}

// handleSiteGames serves GET /api/games: the active games grid. The clock
// snapshot and position come from the same GameView the XRPC layer renders,
// so listGames and the site grid never disagree.
func (a *AppView) handleSiteGames(w http.ResponseWriter, r *http.Request) {
	views, _, err := a.games.ListGames(r.Context(), games.ListParams{
		Status: "active",
		Limit:  100,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
		return
	}
	out := make([]siteGame, 0, len(views))
	for _, v := range views {
		out = append(out, *siteGameOf(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"games": out, "serverTime": a.now().UTC().Format(time.RFC3339Nano)})
}

// now returns the manager's (test-controllable) clock.
func (a *AppView) now() time.Time { return a.games.Now() }

// ---------------------------------------------------------------------------
// GET /api/game?uri=<at-uri>

// siteCommentary is one commentary summary with the site-only receipt
// enrichment (receiptValid/receiptAt joined from the commentary rows) and,
// once the game is finished, the verify-affordance material (ciphertext,
// nonce, keyId).
type siteCommentary struct {
	URI           string  `json:"uri"`
	Ply           int64   `json:"ply"`
	Player        string  `json:"player"`
	Visibility    string  `json:"visibility"`
	Revealed      bool    `json:"revealed"`
	Text          *string `json:"text,omitempty"`
	RevealsAt     *string `json:"revealsAt,omitempty"`
	RevealsAtPly  *int64  `json:"revealsAtPly,omitempty"`
	ReceiptValid  *bool   `json:"receiptValid,omitempty"`
	ReceiptAt     *string `json:"receiptAt,omitempty"`
	AgentPublished bool   `json:"agentPublished"`
	KeyMismatch   bool    `json:"keyMismatch"`
}

// revealedKeys is the reveal-record summary (verify affordance, spec
// §4.7/§8.2): with a key, ciphertext, nonce, and AAD
// ${gameUri}|${ply}|${playerDid} anyone can verify a note locally.
type revealedKey struct {
	Player         string `json:"player"`
	KeyID          string `json:"keyId"`
	Key            string `json:"key"` // base64 raw 32B content key
	AgentPublished bool   `json:"agentPublished"`
	Mismatch       bool   `json:"mismatch"`
}

type siteCiphertext struct {
	Ply        int64  `json:"ply"`
	Player     string `json:"player"`
	Ciphertext string `json:"ciphertext"` // base64
	Nonce      string `json:"nonce"`      // base64
	KeyID      string `json:"keyId"`
	Sha256     string `json:"sha256"` // hex of the ciphertext bytes
}

type siteGameDetail struct {
	siteGame
	DrawOfferDID string            `json:"drawOfferDID,omitempty"`
	History      []siteHistoryItem `json:"history"`
	Commentary   []siteCommentary  `json:"commentary"`
	ServerTime   string            `json:"serverTime"`
	// Reveal carries the post-game key summary; nil until the game is
	// finished AND a reveal record was written.
	Reveal *siteReveal `json:"reveal,omitempty"`
}

type siteHistoryItem struct {
	Ply              int64      `json:"ply"`
	Player           string     `json:"player"`
	San              string     `json:"san,omitempty"`
	Payload          any        `json:"payload"`
	ReceivedAt       string     `json:"receivedAt"`
	ClockRemainingMs *int64     `json:"clockRemainingMs,omitempty"`
	Position         any        `json:"position,omitempty"`
	Clocks           []siteClock `json:"clocks"`
}

type siteReveal struct {
	Reason      string           `json:"reason"`
	Keys        []revealedKey    `json:"keys"`
	Ciphertexts []siteCiphertext `json:"ciphertexts"`
}

// revealSummary assembles the site reveal summary for a finished game from
// the same rows revealAll wrote (the server holds these unwrapped content
// keys; the public reveal record exposes them on the PDS).
func (a *AppView) revealSummary(ctx context.Context, gameURI string, result *siteResult) *siteReveal {
	rows, err := a.repos.Commentary.ListByGame(ctx, gameURI)
	if err != nil {
		return nil
	}
	rev := &siteReveal{Keys: []revealedKey{}, Ciphertexts: []siteCiphertext{}}
	if result != nil {
		rev.Reason = result.Reason
	}
	type keyID struct{ player, keyID string }
	agg := map[keyID]*revealedKey{}
	var order []keyID
	for _, c := range rows {
		player := ""
		if c.PlayerDID != nil {
			player = *c.PlayerDID
		}
		ply := int64(0)
		if c.Ply != nil {
			ply = int64(*c.Ply)
		}
		if len(c.Ciphertext) > 0 {
			sum := sha256.Sum256(c.Ciphertext)
			rev.Ciphertexts = append(rev.Ciphertexts, siteCiphertext{
				Ply:        ply,
				Player:     player,
				Ciphertext: base64.StdEncoding.EncodeToString(c.Ciphertext),
				Nonce:      base64.StdEncoding.EncodeToString(c.Nonce),
				KeyID:      strDeref(c.KeyID),
				Sha256:     hex.EncodeToString(sum[:]),
			})
		}
		if len(c.ContentKey) == 0 || c.KeyID == nil || c.PlayerDID == nil {
			continue
		}
		k := keyID{player: player, keyID: *c.KeyID}
		entry, ok := agg[k]
		if !ok {
			entry = &revealedKey{
				Player: k.player,
				KeyID:  k.keyID,
				Key:    base64.StdEncoding.EncodeToString(c.ContentKey),
			}
			agg[k] = entry
			order = append(order, k)
		}
		entry.AgentPublished = entry.AgentPublished || c.AgentPublished
		entry.Mismatch = entry.Mismatch || c.KeyMismatch
	}
	for _, k := range order {
		rev.Keys = append(rev.Keys, *agg[k])
	}
	return rev
}

// commentaryEnriched merges the getState summaries with the raw commentary
// rows (receipt + publication columns) into the site shape.
func commentaryEnriched(views []games.CommentaryEntry, rows []repo.Commentary) []siteCommentary {
	byRow := map[string]repo.Commentary{}
	for _, r := range rows {
		byRow[r.URI] = r
	}
	out := make([]siteCommentary, 0, len(views))
	for _, v := range views {
		sc := siteCommentary{
			URI:          v.URI,
			Ply:          v.Ply,
			Player:       v.Player,
			Visibility:   v.Visibility,
			Revealed:     v.Revealed,
			Text:         v.Text,
			RevealsAt:    isoOpt(v.RevealsAt),
			RevealsAtPly: v.RevealsAtPly,
		}
		if r, ok := byRow[v.URI]; ok {
			sc.ReceiptValid = r.ReceiptValid
			if r.ReceiptRat != nil {
				t := isoPtr(r.ReceiptRat)
				sc.ReceiptAt = &t
			}
			sc.AgentPublished = r.AgentPublished
			sc.KeyMismatch = r.KeyMismatch
		}
		out = append(out, sc)
	}
	return out
}

func isoOpt(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := isoPtr(t)
	return &s
}

// handleSiteGame serves GET /api/game?uri=<at-uri>: the full getState
// payload shape plus site enrichments.
func (a *AppView) handleSiteGame(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": "uri query parameter is required"})
		return
	}
	ctx := r.Context()
	view, err := a.games.GetState(ctx, games.StateQuery{GameURI: uri})
	if err != nil {
		var ge *games.Error
		if errors.As(err, &ge) && ge.Code == games.CodeNotFound {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "NotFound", "message": "no such game"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
		return
	}

	g := siteGameOf(view)
	detail := &siteGameDetail{
		siteGame:   *g,
		History:    []siteHistoryItem{},
		Commentary: []siteCommentary{},
		ServerTime: isoPtr(&view.ServerTime),
	}
	if view.DrawOfferDID != "" {
		detail.DrawOfferDID = view.DrawOfferDID
	}
	for _, h := range view.History {
		item := siteHistoryItem{
			Ply:              h.Ply,
			Player:           h.Player,
			San:              h.SAN,
			ReceivedAt:       h.ReceivedAt.UTC().Format(time.RFC3339Nano),
			ClockRemainingMs: h.ClockRemainingMs,
			Clocks:           []siteClock{},
		}
		var payload any
		if err := json.Unmarshal(h.Payload, &payload); err == nil {
			item.Payload = payload
		}
		detail.History = append(detail.History, item)
	}
	if pos, ok := games.RenderPosition(view.Position); ok {
		detail.Position = pos
	}

	rows, err := a.repos.Commentary.ListByGame(ctx, uri)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
		return
	}
	detail.Commentary = commentaryEnriched(view.Commentary, rows)
	if view.Status == "finished" {
		detail.Reveal = a.revealSummary(ctx, uri, detail.Result)
	}
	writeJSON(w, http.StatusOK, detail)
}

// ---------------------------------------------------------------------------
// GET /api/actors/{did}

type siteActor struct {
	DID              string            `json:"did"`
	Handle           string            `json:"handle,omitempty"`
	Profile          any               `json:"profile,omitempty"`
	OperatorVerified bool              `json:"operatorVerified"`
	RecentGames      []siteGame        `json:"recentGames"`
	Flags            []siteFlagSummary `json:"flags"`
}

type siteFlagSummary struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// handleSiteActor serves GET /api/actors/{did}: minimal actor page data —
// handle, DID, recent games, and a flags summary.
func (a *AppView) handleSiteActor(w http.ResponseWriter, r *http.Request) {
	did := r.PathValue("did")
	if did == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": "did path parameter is required"})
		return
	}
	ctx := r.Context()
	actor, err := a.repos.Actors.Get(ctx, did)
	if errors.Is(err, repo.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "NotFound", "message": "no such actor"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
		return
	}

	out := &siteActor{
		DID:              actor.DID,
		Handle:           strDeref(actor.Handle),
		OperatorVerified: actor.OperatorVerified,
		RecentGames:      []siteGame{},
		Flags:            []siteFlagSummary{},
	}
	if len(actor.Profile) > 0 {
		var p any
		if json.Unmarshal(actor.Profile, &p) == nil {
			out.Profile = p
		}
	}

	views, _, err := a.games.ListGames(ctx, games.ListParams{Player: did, Limit: 20})
	if err == nil {
		for _, v := range views {
			out.RecentGames = append(out.RecentGames, *siteGameOf(v))
		}
	}

	flags, err := a.repos.Flags.ListBySubject(ctx, did, 100)
	if err == nil {
		counts := map[string]*siteFlagSummary{}
		var order []string
		for _, f := range flags {
			key := f.Kind + "|" + f.Severity
			if _, ok := counts[key]; !ok {
				counts[key] = &siteFlagSummary{Kind: f.Kind, Severity: f.Severity}
				order = append(order, key)
			}
			counts[key].Count++
		}
		sort.Strings(order)
		for _, k := range order {
			out.Flags = append(out.Flags, *counts[k])
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /api/challenges

type siteChallenge struct {
	ID              string `json:"id"`
	ChallengerDID   string `json:"challengerDID"`
	GameType        string `json:"gameType"`
	Variant         *string `json:"variant,omitempty"`
	TimeControl     any    `json:"timeControl"`
	SeatPreference  string `json:"seatPreference,omitempty"`
	Rated           bool   `json:"rated"`
	Status          string `json:"status"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
}

// handleSiteChallenges serves GET /api/challenges: the open-challenge
// lobby strip. Only open challenges (opponent IS NULL) are listed.
func (a *AppView) handleSiteChallenges(w http.ResponseWriter, r *http.Request) {
	open := true
	if r.URL.Query().Get("open") == "false" {
		open = false
	}
	rows, err := a.repos.Challenges.List(r.Context(), repo.ChallengesFilter{
		Open:  &open,
		Limit: 50,
	}, a.now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
		return
	}
	out := make([]siteChallenge, 0, len(rows))
	for _, c := range rows {
		var tc any
		if len(c.TimeControl) > 0 {
			if json.Unmarshal(c.TimeControl, &tc) != nil {
				tc = nil
			}
		}
		out = append(out, siteChallenge{
			ID:             c.ID,
			ChallengerDID:  c.ChallengerDID,
			GameType:       c.GameType,
			Variant:        c.Variant,
			TimeControl:    tc,
			SeatPreference: c.SeatPreference,
			Rated:          c.Rated,
			Status:         c.Status,
			ExpiresAt:      isoPtr(c.ExpiresAt),
			CreatedAt:      isoPtr(c.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenges": out})
}

// ---------------------------------------------------------------------------
// GET /docs — embedded lexicon markdown + the policies page (§7, §8.3)

//go:embed docs_content/*.md docs_content/lexicons/*.md
var docsFS embed.FS

// docsIndexEntry is one lexicon doc in the /docs listing.
type docsIndexEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// handleDocsPage serves the /docs index: the lexicon doc listing plus the
// embedded policies page content. The frontend renders the markdown.
func (a *AppView) handleDocsPage(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(r.URL.Path, "/docs") != "" && r.URL.Path != "/docs" {
		a.handleDocsAsset(w, r)
		return
	}
	entries, err := fs.ReadDir(docsFS, "docs_content/lexicons")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
		return
	}
	lexicons := []docsIndexEntry{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		lexicons = append(lexicons, docsIndexEntry{
			Name: strings.TrimSuffix(e.Name(), ".md"),
			Path: path.Join("/docs/lexicons", e.Name()),
		})
	}
	sort.Slice(lexicons, func(i, j int) bool { return lexicons[i].Name < lexicons[j].Name })

	policies, _ := fs.ReadFile(docsFS, "docs_content/policies.md")
	writeJSON(w, http.StatusOK, map[string]any{
		"lexicons": lexicons,
		"policies": string(policies),
	})
}

// handleDocsAsset serves one embedded markdown file (a lexicon page).
func (a *AppView) handleDocsAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	data, err := docsFS.ReadFile("docs_content/lexicons/" + name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "NotFound"})
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write(data)
}
