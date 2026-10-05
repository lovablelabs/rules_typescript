package dev_server_test

import (
	"os"
	"path/filepath"
	"testing"
)

// macOS TMPDIR sits beneath /var -> /private/var; a deeper real directory behind
// the link makes lexical and resolved paths disagree on Linux the same way.
func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "symlinked-tmp-")
	if err != nil {
		panic(err)
	}
	real := filepath.Join(base, "private", "tmp")
	if err := os.MkdirAll(real, 0o755); err != nil {
		panic(err)
	}
	link := filepath.Join(base, "tmp")
	if err := os.Symlink(real, link); err != nil {
		panic(err)
	}
	os.Setenv("TMPDIR", link)
	os.Setenv("GOTMPDIR", link)
	code := m.Run()
	os.RemoveAll(base)
	os.Exit(code)
}
