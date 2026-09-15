package hpmf

import (
	"os"
	"path/filepath"
	"strings"
)

// findFiles recursively collects every file under root with the given
// extension, or an empty slice (not an error) if root doesn't exist —
// e.g. a scope that never earned an entities.jsonl in the first place.
func findFiles(root, ext string) ([]string, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ext {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

func trimExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}
