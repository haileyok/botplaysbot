// Command gen regenerates Go types and XRPC client code from the lexicons/
// directory using github.com/jcalabro/atmos/cmd/lexgen.
//
// Wire it up with:
//
//	//go:generate go run .
//
// run from this directory (i.e. `go generate ./internal/tools/gen`), or run it
// directly: `go run ./internal/tools/gen`.
//
// Flags:
//
//	-outroot DIR  write outputs under DIR instead of the repository root
//	              (contents are identical; used by scripts/check-codegen.sh
//	              to verify generated code freshness without touching the tree)
//	-lexgen PATH  path to the lexgen config (default: lexgen.json next to this file)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jcalabro/atmos/lexgen"
)

//go:generate go run .


// lexgenVersion pins the code generator; must match the atmos version in go.mod.
const lexgenVersion = "v0.4.0"

func main() {
	outroot := flag.String("outroot", "", "redirect all outputs under this directory")
	lexgenCfg := flag.String("lexgen", "", "path to lexgen config JSON")
	flag.Parse()

	if err := run(*outroot, *lexgenCfg); err != nil {
		fmt.Fprintf(os.Stderr, "gen: %v\n", err)
		os.Exit(1)
	}
}

func run(outroot, lexgenCfg string) error {
	// Locate the repository root so paths are CWD-independent.
	root, err := findRepoRoot()
	if err != nil {
		return err
	}

	if lexgenCfg == "" {
		lexgenCfg = filepath.Join(root, "internal", "tools", "gen", "lexgen.json")
	} else if !filepath.IsAbs(lexgenCfg) {
		lexgenCfg, err = filepath.Abs(lexgenCfg)
		if err != nil {
			return err
		}
	}

	raw, err := os.ReadFile(lexgenCfg)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg lexgen.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	// Rewrite every output path as absolute (relative to the repo root, or to
	// -outroot). lexgen writes files relative to its CWD; absolute paths make
	// this deterministic and let -outroot redirect for freshness checks.
	abs := func(p string) string {
		base := root
		if outroot != "" {
			base = outroot
		}
		return filepath.Join(base, p)
	}
	for i := range cfg.Packages {
		cfg.Packages[i].OutDir = abs(cfg.Packages[i].OutDir)
	}
	if cfg.SharedTypesDir != "" {
		cfg.SharedTypesDir = abs(cfg.SharedTypesDir)
	}

	tmpCfg, err := writeTempConfig(&cfg)
	if err != nil {
		return err
	}
	defer os.Remove(tmpCfg)

	cmd := exec.Command("go", "run",
		"github.com/jcalabro/atmos/cmd/lexgen@"+lexgenVersion,
		"-lexdir", filepath.Join(root, "lexicons"),
		"-config", tmpCfg,
	)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lexgen: %w", err)
	}
	return nil
}

func writeTempConfig(cfg *lexgen.Config) (string, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "lexgen-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// findRepoRoot walks up from the CWD looking for go.mod.
func findRepoRoot() (string, error) {
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
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
