package incremental

import (
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"
)

// A skipped run is only sound if every input change moves the digest.
func TestDigestSeesEveryInputChange(t *testing.T) {
	root := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/BUILD.bazel", "")
	write("a/b/index.ts", "export {};\n")
	write("node_modules/x/index.js", "")
	extra := filepath.Join(root, "node_modules", ".modules.yaml")
	opts := Options{Root: root, Skip: func(rel string) bool { return rel == "node_modules" }, Extra: []string{extra}, Key: []string{"-mode=fix"}}
	digest := func() string {
		d, err := Digest(opts)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	last := digest()
	if again := digest(); again != last {
		t.Fatalf("an unchanged tree moved the digest: %s then %s", last, again)
	}
	future := time.Now().Add(time.Hour)
	for _, change := range []struct {
		name string
		do   func()
	}{
		{"edit", func() { write("a/b/index.ts", "export const x = 1;\n") }},
		{"same-size edit", func() {
			write("a/b/index.ts", "export const y = 1;\n")
			os.Chtimes(filepath.Join(root, "a/b/index.ts"), future, future)
		}},
		{"new file", func() { write("a/b/other.ts", "") }},
		{"new directory", func() { write("c/BUILD.bazel", "") }},
		{"removed file", func() { os.Remove(filepath.Join(root, "a/b/other.ts")) }},
		{"extra appears", func() { write("node_modules/.modules.yaml", "") }},
		{"key", func() { opts.Key = []string{"-mode=diff"} }},
	} {
		change.do()
		next := digest()
		if next == last {
			t.Errorf("%s left the digest at %s", change.name, next)
		}
		last = next
	}
	write("node_modules/x/index.js", "changed")
	if next := digest(); next != last {
		t.Errorf("a skipped subtree moved the digest")
	}
	opts.ByContent = func(rel string) bool { return path.Base(rel) == "BUILD.bazel" }
	last = digest()
	write("a/BUILD.bazel", "")
	os.Chtimes(filepath.Join(root, "a/BUILD.bazel"), future, future)
	if next := digest(); next != last {
		t.Errorf("rewriting a BUILD file with the same bytes moved the digest")
	}
	write("a/BUILD.bazel", "x")
	if next := digest(); next == last {
		t.Errorf("a BUILD content change left the digest")
	}
}
