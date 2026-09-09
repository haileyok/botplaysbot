// Package testutil wires integration tests to real dependencies:
//
//   - Postgres: read from DATABASE_URL (docker compose up -d db); tests skip
//     with a clear message when it is absent or unreachable.
//   - The dev PDS harness: launches node packages/dev/pds-harness.mjs
//     (in-memory PDS + PLC) and speaks its control API; tests skip when Node
//     or the harness is unavailable.
//
// Everything here is test tooling; nothing in this package ships in the
// server binary.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/botplaysbot/internal/db"
)

// PostgresURL returns DATABASE_URL, skipping the test when unset or the
// database cannot be reached within a bounded window.
func PostgresURL(t testingT) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("integration test: DATABASE_URL is not set (start Postgres with: docker compose up -d db)")
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deadline := time.Now().Add(20 * time.Second)
	for {
		pool, err := db.OpenPool(ctx, url)
		if err == nil {
			err = db.Ping(ctx, pool)
			pool.Close()
		}
		if err == nil {
			return url
		}
		if time.Now().After(deadline) {
			t.Skipf("integration test: Postgres at DATABASE_URL unreachable: %v", err)
			return ""
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Account is an agent account created via the harness control API.
type Account struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	Password    string `json:"password"`
	AppPassword string `json:"appPassword"`
}

// PDSHarness is a running packages/dev/pds-harness.mjs process.
type PDSHarness struct {
	cmd      *exec.Cmd
	stopOnce sync.Once
	PDS      string // PDS base URL, e.g. http://localhost:41234
	PLC      string // PLC directory base URL (point PLAYSBOT_PLC_DIRECTORY_URL here)
	Control  string // control API base URL
}

// harnessReadyPrefix is the marker line the harness prints on stdout.
const harnessReadyPrefix = "playsbot-pds-harness ready "

// repoRoot walks up from dir until a go.mod is found.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("testutil: no go.mod above %s", dir)
		}
		dir = parent
	}
}

// StartPDS launches the dev PDS harness and waits (bounded) for readiness.
// Registers cleanup with t. Skips the test when Node or the harness script
// is unavailable.
func StartPDS(t testingT) *PDSHarness {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("integration test: node not found in PATH; the dev PDS harness cannot start")
		return nil
	}
	root, err := repoRoot()
	if err != nil {
		t.Skipf("integration test: cannot locate repository root: %v", err)
		return nil
	}
	script := filepath.Join(root, "packages", "dev", "pds-harness.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("integration test: harness script missing: %s", script)
		return nil
	}
	// node_modules must be installed for @atproto/dev-env to resolve.
	if _, err := os.Stat(filepath.Join(root, "packages", "dev", "node_modules", "@atproto", "dev-env")); err != nil {
		t.Skip("integration test: @atproto/dev-env not installed (run: pnpm install)")
		return nil
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("testutil: free port: %v", err)
	}

	cmd := exec.Command(node, script, "--port", fmt.Sprint(port))
	cmd.Dir = root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("testutil: stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("testutil: start harness: %v", err)
	}

	h := &PDSHarness{cmd: cmd}

	ready := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		var sb strings.Builder
		buf := make([]byte, 1024)
		for {
			if time.Now().After(deadline) {
				ready <- fmt.Errorf("harness not ready in 60s; output so far: %s", sb.String())
				return
			}
			n, err := stdout.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if line, _, ok := strings.Cut(sb.String(), "\n"); ok {
					if rest, found := strings.CutPrefix(line, harnessReadyPrefix); found {
						var urls struct {
							PDS     string `json:"pds"`
							PLC     string `json:"plc"`
							Control string `json:"control"`
						}
						if jerr := json.Unmarshal([]byte(rest), &urls); jerr != nil {
							ready <- fmt.Errorf("harness ready line unparsable: %w", jerr)
							return
						}
						h.PDS = urls.PDS
						h.PLC = urls.PLC
						h.Control = urls.Control
						ready <- nil
						return
					}
				}
			}
			if err != nil {
				ready <- fmt.Errorf("harness exited before readiness: %v; output: %s", err, sb.String())
				return
			}
		}
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Skipf("integration test: dev PDS harness failed: %v", err)
			return nil
		}
	case <-time.After(70 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Skip("integration test: dev PDS harness timed out")
		return nil
	}

	t.Cleanup(h.stop)
	return h
}

// stop shuts the harness down politely, then forcefully.
func (h *PDSHarness) stop() {
	if h.Control != "" {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(h.Control+"/shutdown", "application/json", nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	done := make(chan struct{})
	go func() {
		_, _ = h.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

// Healthz reports whether the harness control API is answering.
func (h *PDSHarness) Healthz() bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(h.Control + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// Shutdown stops the harness before test cleanup. Safe to call twice.
func (h *PDSHarness) Shutdown() {
	h.stopOnce.Do(h.stop)
}

// CreateAccount creates an agent account via the harness control API.
func (h *PDSHarness) CreateAccount(t testingT) Account {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(h.Control+"/accounts", "application/json", nil)
	if err != nil {
		t.Fatalf("testutil: create account: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("testutil: create account: status %d: %s", resp.StatusCode, body)
	}
	var account Account
	if err := json.Unmarshal(body, &account); err != nil {
		t.Fatalf("testutil: create account response: %v", err)
	}
	return account
}

// freePort asks the kernel for a free TCP port (racy but good enough for
// test listeners).
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// testingT is the minimal testing interface used here.
type testingT interface {
	Helper()
	Cleanup(func())
	Skip(args ...any)
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}
