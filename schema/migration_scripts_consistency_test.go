// Package schema holds no Go code of its own — just the migration SQL
// files and the two shell scripts that apply them. This test file exists
// purely to let `go test ./...` (and CI) catch the one thing those two
// scripts can't catch about themselves: silent divergence between them.
package schema

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMigrationScripts_AgreeOnFilesAndProbes is the real regression test
// for review finding B24: schema/migrate.sh and install.sh deliberately
// each implement their own migration-list-plus-idempotency-probe logic
// (see migrate.sh's own doc comment on why: one is a bare-metal bash
// script with real TTY/prompt handling, the other a sh-only, non-
// interactive container entrypoint with none — sharing code between them
// would mean threading interactivity concerns through logic that has
// none). That duplication is a deliberate, reasonable choice, not the
// bug — but nothing previously caught the two scripts silently drifting
// apart, e.g. a migration added to one and forgotten in the other, or
// the same migration probed with different (and possibly wrong) SQL in
// each. This test parses both files' migration lists and probe
// functions and confirms they agree on both the exact set of files and
// the exact probe SQL for each.
func TestMigrationScripts_AgreeOnFilesAndProbes(t *testing.T) {
	migrateSh, err := os.ReadFile("migrate.sh")
	if err != nil {
		t.Fatalf("read migrate.sh: %v", err)
	}
	installSh, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	migrateList, migrateProbes := parseMigrationScript(t, "migrate.sh", string(migrateSh))
	installList, installProbes := parseMigrationScript(t, "install.sh", string(installSh))

	diskFiles, err := realMigrationFiles()
	if err != nil {
		t.Fatalf("list schema/*.sql: %v", err)
	}

	assertSameStringSlice(t, "migrate.sh's migration list", migrateList, "the real schema/*.sql files on disk", diskFiles)
	assertSameStringSlice(t, "install.sh's migration list", installList, "the real schema/*.sql files on disk", diskFiles)

	assertSameKeys(t, "migrate.sh's probe() cases", migrateProbes, "migrate.sh's own migration list", migrateList)
	assertSameKeys(t, "install.sh's migration_probe() cases", installProbes, "install.sh's own migration list", installList)

	for _, f := range diskFiles {
		mp, mOK := migrateProbes[f]
		ip, iOK := installProbes[f]
		if !mOK || !iOK {
			continue // already reported by assertSameKeys above
		}
		if mp != ip {
			t.Errorf("probe for %s differs between the two scripts:\n  migrate.sh: %s\n  install.sh: %s", f, mp, ip)
		}
	}
}

var (
	forLoopRe   = regexp.MustCompile(`(?s)for f in (.*?);\s*do`)
	probeCaseRe = regexp.MustCompile(`(?m)^\s*([0-9A-Za-z_.]+\.sql)\)\s*echo\s+"((?:[^"\\]|\\.)*)"`)
)

// parseMigrationScript extracts the ordered `for f in ...; do` file list
// and the `<probe func>() { case "$1" in ... esac }` filename->SQL map
// from one of the two migration scripts' source text.
func parseMigrationScript(t *testing.T, name, src string) (list []string, probes map[string]string) {
	t.Helper()

	m := forLoopRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s: could not find a `for f in ...; do` migration list", name)
	}
	for _, tok := range strings.Fields(strings.ReplaceAll(m[1], `\`, " ")) {
		list = append(list, tok)
	}

	probes = map[string]string{}
	for _, m := range probeCaseRe.FindAllStringSubmatch(src, -1) {
		probes[m[1]] = m[2]
	}
	if len(probes) == 0 {
		t.Fatalf("%s: could not find any probe cases", name)
	}
	return list, probes
}

func realMigrationFiles() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

func assertSameStringSlice(t *testing.T, aName string, a []string, bName string, b []string) {
	t.Helper()
	aSet, bSet := map[string]bool{}, map[string]bool{}
	for _, s := range a {
		aSet[s] = true
	}
	for _, s := range b {
		bSet[s] = true
	}
	for s := range aSet {
		if !bSet[s] {
			t.Errorf("%s includes %s, but %s does not", aName, s, bName)
		}
	}
	for s := range bSet {
		if !aSet[s] {
			t.Errorf("%s includes %s, but %s does not", bName, s, aName)
		}
	}
}

func assertSameKeys(t *testing.T, aName string, a map[string]string, bName string, b []string) {
	t.Helper()
	bSet := map[string]bool{}
	for _, s := range b {
		bSet[s] = true
	}
	for k := range a {
		if !bSet[k] {
			t.Errorf("%s has a probe for %s, but %s does not list it", aName, k, bName)
		}
	}
	for _, s := range b {
		if _, ok := a[s]; !ok {
			t.Errorf("%s lists %s, but %s has no probe for it", bName, s, aName)
		}
	}
}
