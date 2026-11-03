package testdata

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed all:logging
var fixtures embed.FS

var (
	extractOnce sync.Once
	fixtureDir  string
)

// FixturePath returns the on-disk path of an embedded fixture file or directory.
// Fixtures are extracted to a temporary directory on first use because the tests
// hand them to external processes (oc, shell scripts) that need real files.
// It panics if the path does not exist in the embedded tree. Paths are relative
// to this directory, so they start at "logging".
func FixturePath(elem ...string) string {
	rel := path.Join(elem...)
	if _, err := fs.Stat(fixtures, rel); err != nil {
		msg := fmt.Sprintf("fixture not found: %q", rel)
		if strings.HasPrefix(rel, "testdata/") {
			msg += `: paths are relative to test/e2e/testdata, drop the "testdata" prefix`
		}
		panic(msg)
	}

	extractOnce.Do(extractFixtures)
	return filepath.Join(fixtureDir, filepath.FromSlash(rel))
}

func extractFixtures() {
	dir, err := os.MkdirTemp("", "testdata-fixtures-")
	if err != nil {
		panic(fmt.Sprintf("failed to create fixture directory: %v", err))
	}

	err = fs.WalkDir(fixtures, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fixtures.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		panic(fmt.Sprintf("failed to extract fixtures: %v", err))
	}
	fixtureDir = dir
}
