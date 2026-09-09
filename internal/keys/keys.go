// Package keys manages the AppView service identity keys:
//
//   - The Ed25519 service signing key, used to sign moveTokens (spec §2.2,
//     Appendix B) and record signatures. It lives in a PEM file on disk
//     (default data/service-signing.key, mode 0600) and is generated on
//     first boot.
//
//   - The X25519 commentary-escrow keypair (spec §8.1). The public key is
//     published via /.well-known/plays-bot/escrow-keys.json and referenced
//     from the service DID document as #commentary-escrow-<rotationId>.
//     Rotations are stored in the escrow_keys table; the current key is
//     reused across restarts so agents can keep encrypting to it.
package keys

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/botplaysbot/internal/tid"
)

// signingKeyPEMType is the PEM block type for the service signing key. The
// DER payload is PKCS#8.
const signingKeyPEMType = "PLAYSBOT SERVICE SIGNING KEY"

// ErrNotFound is returned by directory lookups for an unknown rotationId.
var ErrNotFound = errors.New("keys: escrow key not found")

// LoadOrCreateSigningKey returns the Ed25519 private key stored at path,
// generating and persisting a new one (mode 0600, creating parent
// directories) when the file does not exist.
func LoadOrCreateSigningKey(path string) (ed25519.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		key, err := parseSigningKey(data)
		if err != nil {
			return nil, fmt.Errorf("keys: parse %s: %w", path, err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("keys: read %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("keys: generate ed25519: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("keys: marshal pkcs8: %w", err)
	}
	block := &pem.Block{Type: signingKeyPEMType, Bytes: der}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("keys: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("keys: write %s: %w", path, err)
	}

	// Enforce the mode explicitly: a pre-existing umask could have widened
	// the file above 0600 at creation time.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("keys: chmod %s: %w", path, err)
	}
	return priv, nil
}

func parseSigningKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse pkcs8: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ed25519 key (got %T)", parsed)
	}
	return priv, nil
}

// SigningPublicKeyB64 returns the base64url encoding of the raw 32-byte
// Ed25519 public key, as published in service.json.
func SigningPublicKeyB64(priv ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// EscrowKey is one X25519 escrow key rotation.
type EscrowKey struct {
	// RotationID is an opaque identifier (a TID). Agents reference it in
	// commentary escrowKey metadata.
	RotationID string
	// PublicKey is the raw 32-byte X25519 public key.
	PublicKey [32]byte
	// PrivateKey is held by the AppView; nil after a rotation's
	// destroy_after deadline has been processed (Phase F scope).
	PrivateKey *[32]byte
	// ActiveFrom/ActiveTo bound the rotation window.
	ActiveFrom time.Time
	ActiveTo   *time.Time
}

// EscrowKeyDirectory provides escrow key rotations. Implementations must be
// safe for concurrent use.
type EscrowKeyDirectory interface {
	// Current returns the most recent rotation that still holds a private key.
	Current(ctx context.Context) (*EscrowKey, error)
	// Get returns the rotation with the given rotationId.
	Get(ctx context.Context, rotationID string) (*EscrowKey, error)
	// List returns all rotations, most recent first.
	List(ctx context.Context) ([]EscrowKey, error)
}

// DBDirectory is the Postgres-backed EscrowKeyDirectory.
type DBDirectory struct {
	pool *pgxpool.Pool
}

// NewDBDirectory returns a DB-backed escrow key directory.
func NewDBDirectory(pool *pgxpool.Pool) *DBDirectory {
	return &DBDirectory{pool: pool}
}

const escrowColumns = `rotation_id, public_key, private_key, active_from, active_to`

func scanEscrowKey(row pgx.Row) (*EscrowKey, error) {
	var (
		k        EscrowKey
		pub      []byte
		priv     []byte
		activeTo *time.Time
	)
	if err := row.Scan(&k.RotationID, &pub, &priv, &k.ActiveFrom, &activeTo); err != nil {
		return nil, err
	}
	if len(pub) != 32 {
		return nil, fmt.Errorf("keys: rotation %s: public key is %d bytes, want 32", k.RotationID, len(pub))
	}
	copy(k.PublicKey[:], pub)
	if priv != nil {
		if len(priv) != 32 {
			return nil, fmt.Errorf("keys: rotation %s: private key is %d bytes, want 32", k.RotationID, len(priv))
		}
		p := [32]byte{}
		copy(p[:], priv)
		k.PrivateKey = &p
	}
	k.ActiveTo = activeTo
	return &k, nil
}

// Current returns the most recent rotation that still holds a private key.
func (d *DBDirectory) Current(ctx context.Context) (*EscrowKey, error) {
	k, err := scanEscrowKey(d.pool.QueryRow(ctx,
		`SELECT `+escrowColumns+` FROM escrow_keys WHERE private_key IS NOT NULL ORDER BY active_from DESC, rotation_id DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// Get returns the rotation with the given rotationId.
func (d *DBDirectory) Get(ctx context.Context, rotationID string) (*EscrowKey, error) {
	k, err := scanEscrowKey(d.pool.QueryRow(ctx,
		`SELECT `+escrowColumns+` FROM escrow_keys WHERE rotation_id = $1`, rotationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// List returns all rotations, most recent first.
func (d *DBDirectory) List(ctx context.Context) ([]EscrowKey, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT `+escrowColumns+` FROM escrow_keys ORDER BY active_from DESC, rotation_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("keys: list escrow keys: %w", err)
	}
	defer rows.Close()

	var out []EscrowKey
	for rows.Next() {
		k, err := scanEscrowKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// EnsureCurrent returns the current escrow rotation, generating and storing a
// new X25519 keypair (rotationId = fresh TID) when none exists. Keys persist
// in escrow_keys, so restarting against the same database reuses the same key.
func EnsureCurrent(ctx context.Context, dir EscrowKeyDirectory) (*EscrowKey, error) {
	k, err := dir.Current(ctx)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return GenerateCurrent(ctx, dir)
}

// GenerateCurrent creates a new X25519 escrow keypair with a fresh TID
// rotationId and persists it as the newest rotation.
func GenerateCurrent(ctx context.Context, dir EscrowKeyDirectory) (*EscrowKey, error) {
	dbDir, ok := dir.(*DBDirectory)
	if !ok {
		return nil, fmt.Errorf("keys: GenerateCurrent requires a *DBDirectory, got %T", dir)
	}

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("keys: generate x25519: %w", err)
	}
	k := &EscrowKey{
		RotationID: tid.Next().String(),
		ActiveFrom: time.Now().UTC(),
	}
	copy(k.PublicKey[:], priv.PublicKey().Bytes())
	p := [32]byte{}
	copy(p[:], priv.Bytes())
	k.PrivateKey = &p

	_, err = dbDir.pool.Exec(ctx,
		`INSERT INTO escrow_keys (rotation_id, public_key, private_key, active_from)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (rotation_id) DO NOTHING`,
		k.RotationID, k.PublicKey[:], k.PrivateKey[:], k.ActiveFrom)
	if err != nil {
		return nil, fmt.Errorf("keys: insert escrow key: %w", err)
	}
	return k, nil
}
