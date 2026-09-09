package keys

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/haileyok/botplaysbot/internal/testutil"
)

func TestLoadOrCreateSigningKeyGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "service-signing.key")

	priv1, err := LoadOrCreateSigningKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}

	// Second load must return the same key: it persists across restarts.
	priv2, err := LoadOrCreateSigningKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !ed25519.PrivateKey(priv1).Equal(ed25519.PrivateKey(priv2)) {
		t.Fatal("reload returned a different key")
	}

	if got, want := len(SigningPublicKeyB64(priv1)), 43; got != want {
		t.Fatalf("public key b64 length = %d, want %d", got, want)
	}
}

func TestLoadOrCreateSigningKeyRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service-signing.key")
	if err := os.WriteFile(path, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigningKey(path); err == nil {
		t.Fatal("expected an error for a non-PEM key file")
	}
}

// TestEscrowDirectoryLifecycle runs against a live Postgres when DATABASE_URL
// points at one; it skips with a clear message otherwise. Each run gets an
// isolated, freshly migrated throwaway database (testutil.IsolatedDB), so it
// cannot collide with the appview integration suite running in parallel (the
// integration suite in internal/appview covers the DB-backed flow end to end).
func TestEscrowDirectoryLifecycle(t *testing.T) {
	pool := testutil.IsolatedDB(t)
	defer pool.Close()
	ctx := context.Background()

	dir := NewDBDirectory(pool)

	k1, err := EnsureCurrent(ctx, dir)
	if err != nil {
		t.Fatalf("ensure current: %v", err)
	}
	if k1.PrivateKey == nil {
		t.Fatal("new escrow key has no private key")
	}

	// Reuse on second boot.
	k2, err := EnsureCurrent(ctx, dir)
	if err != nil {
		t.Fatalf("ensure current again: %v", err)
	}
	if k2.RotationID != k1.RotationID {
		t.Fatalf("rotation id changed across boots: %s -> %s", k1.RotationID, k2.RotationID)
	}

	// Get and List.
	got, err := dir.Get(ctx, k1.RotationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PublicKey != k1.PublicKey {
		t.Fatal("Get returned a different public key")
	}
	if _, err := dir.Get(ctx, "nonexistent"); err == nil {
		t.Fatal("expected ErrNotFound for unknown rotation")
	}
	all, err := dir.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) == 0 || all[0].RotationID != k1.RotationID {
		t.Fatalf("list = %d keys, want the current one first", len(all))
	}

	// A generated rotation becomes current.
	k3, err := GenerateCurrent(ctx, dir)
	if err != nil {
		t.Fatalf("generate current: %v", err)
	}
	cur, err := dir.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.RotationID != k3.RotationID {
		t.Fatalf("current = %s, want newest rotation %s", cur.RotationID, k3.RotationID)
	}
}
