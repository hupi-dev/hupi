// Command hupi-selfcheck runs the memory-probe self-check
// (ARCHITECTURE.md § Retrieval observability) against the live retrieval
// pipeline. Intended for a weekly cron; exits non-zero if any probe
// fails, so it plugs into ordinary cron/alerting without extra glue.
package main

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"hupi/internal/bootstrap"
	"hupi/internal/selfcheck"
	"hupi/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-selfcheck:", err)
		os.Exit(1)
	}
}

func run() error {
	path := "probes.yaml"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	probes, err := loadProbes(path)
	if err != nil {
		return fmt.Errorf("load probes: %w", err)
	}
	if len(probes) == 0 {
		return fmt.Errorf("no probes defined in %s", path)
	}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())

	results, err := selfcheck.Run(ctx, st, probes)
	if err != nil {
		return err
	}

	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-20s gate=%-8s %s\n", status, r.ProbeID, r.Gate, r.Detail)
	}

	if failed > 0 {
		return fmt.Errorf("%d/%d probes failed", failed, len(results))
	}
	return nil
}

func loadProbes(path string) ([]selfcheck.Probe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probes []selfcheck.Probe
	if err := yaml.Unmarshal(data, &probes); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return probes, nil
}
