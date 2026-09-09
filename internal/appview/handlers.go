package appview

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/haileyok/botplaysbot/internal/db"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/web"
)

// writeJSON emits a JSON response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// handleHealthz reports process and database status. The database check is
// advisory for load balancers: the app returns 503 only when the pool cannot
// be reached.
func (a *AppView) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(r.Context(), a.pool); err != nil {
		a.logger.Error("healthz: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "db": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db": "ok"})
}

// Well-known identity documents (documented in README.md):
//
//	GET /.well-known/plays-bot/service.json
//	  {"did": "<SERVICE_DID>", "signingPublicKey": "<base64url raw 32B Ed25519>"}
//
//	GET /.well-known/plays-bot/escrow-keys.json
//	  {"keys": [{"rotationId": "<TID>", "publicKey": "<base64url raw 32B X25519>",
//	            "algorithm": "X25519", "createdAt": "<RFC3339>"}],
//	   "current": "<rotationId>"}
//
// escrow-keys.json carries the current rotation first, then up to two
// previous rotations so agents can decrypt commentary sealed to recent keys
// until those games finish. Consumers MUST encrypt new commentary to
// keys[current].
type escrowPublicKeyDoc struct {
	RotationID string `json:"rotationId"`
	PublicKey  string `json:"publicKey"`
	Algorithm  string `json:"algorithm"`
	CreatedAt  string `json:"createdAt"`
}

type escrowKeysDoc struct {
	Keys    []escrowPublicKeyDoc `json:"keys"`
	Current string               `json:"current"`
}

// maxEscrowDocs bounds the published rotations: current + two previous.
const maxEscrowDocs = 3

func (a *AppView) handleEscrowKeys() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := a.escrow.Current(r.Context())
		if err != nil {
			a.logger.Error("escrow-keys.json: no current key", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
			return
		}

		all, err := a.escrow.List(r.Context())
		if err != nil {
			a.logger.Error("escrow-keys.json: list failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
			return
		}

		doc := escrowKeysDoc{Current: current.RotationID}
		add := func(k keys.EscrowKey) {
			if len(doc.Keys) >= maxEscrowDocs {
				return
			}
			doc.Keys = append(doc.Keys, escrowPublicKeyDoc{
				RotationID: k.RotationID,
				PublicKey:  base64.RawURLEncoding.EncodeToString(k.PublicKey[:]),
				Algorithm:  "X25519",
				CreatedAt:  k.ActiveFrom.UTC().Format(time.RFC3339Nano),
			})
		}
		add(*current)
		for _, k := range all {
			if k.RotationID == current.RotationID {
				continue
			}
			add(k)
		}
		writeJSON(w, http.StatusOK, doc)
	})
}

type serviceDoc struct {
	DID              string `json:"did"`
	SigningPublicKey string `json:"signingPublicKey"`
}

func (a *AppView) handleServiceDoc() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, serviceDoc{
			DID:              a.cfg.ServiceDID,
			SigningPublicKey: keys.SigningPublicKeyB64(a.signingKey),
		})
	})
}

// webHandler serves the embedded frontend build.
func webHandler() http.Handler {
	return web.Handler()
}
