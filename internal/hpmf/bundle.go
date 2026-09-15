package hpmf

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"hupi/internal/crypto"
	"hupi/internal/identity"
)

// ExportBundle writes every scope in scopes into a fresh temp directory —
// flat (dir ".") if there's exactly one, matching the original
// single-owner MEMORY_FORMAT.md layout, or nested under
// "scopes/<kind>-<owner>" per scope otherwise (docs/GAP_CLOSURE_PLAN.md
// §4.2) — plus a top-level manifest.json. The caller is responsible for
// packing the returned directory (PackAndEncrypt) and removing it
// afterward; this never leaves a plaintext directory as the long-lived
// artifact itself (MEMORY_FORMAT.md § Storage & encryption model).
func ExportBundle(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scopes []identity.Scope, actor string) (tempDir string, manifest Manifest, err error) {
	if len(scopes) == 0 {
		return "", Manifest{}, fmt.Errorf("hpmf: no scopes to export")
	}

	tempDir, err = os.MkdirTemp("", "hupi-export-*")
	if err != nil {
		return "", Manifest{}, fmt.Errorf("hpmf: create temp directory: %w", err)
	}
	// Only cleaned up here on failure — on success, the caller (about to
	// pack it) is responsible for removing it once packing is done.
	success := false
	defer func() {
		if !success {
			os.RemoveAll(tempDir)
		}
	}()

	flat := len(scopes) == 1
	manifest = Manifest{HPMFVersion: HPMFVersion, CreatedAt: time.Now(), CreatedBy: actor}

	for _, scope := range scopes {
		relDir := "."
		if !flat {
			relDir = dirName(scope)
		}
		sm, err := ExportScope(ctx, db, keys, scope, filepath.Join(tempDir, relDir), actor)
		if err != nil {
			return "", Manifest{}, fmt.Errorf("hpmf: export %s:%s: %w", scope.Kind, scope.Owner, err)
		}
		sm.Dir = relDir
		manifest.Scopes = append(manifest.Scopes, sm)
	}

	if err := writeJSONFile(filepath.Join(tempDir, "manifest.json"), manifest); err != nil {
		return "", Manifest{}, err
	}

	success = true
	return tempDir, manifest, nil
}

// ReadManifest loads manifest.json from a decrypted bundle directory
// (the output of DecryptAndUnpack) and checks HPMFVersion — hupi-import
// only ever loads a bundle from the same major version of hupi-export
// that produced it (docs/GAP_CLOSURE_PLAN.md §2).
func ReadManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("hpmf: read manifest.json: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("hpmf: parse manifest.json: %w", err)
	}
	if m.HPMFVersion == "" {
		return Manifest{}, fmt.Errorf("hpmf: manifest.json has no hpmf_version — not a valid HPMF bundle")
	}
	wantMajor := m.HPMFVersion[:1]
	if wantMajor != HPMFVersion[:1] {
		return Manifest{}, fmt.Errorf("hpmf: bundle is HPMF version %s, this build only supports major version %s.x", m.HPMFVersion, HPMFVersion[:1])
	}
	return m, nil
}
