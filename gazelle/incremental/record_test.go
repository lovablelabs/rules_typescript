package incremental

import (
	"os"
	"path"
	"path/filepath"
	"testing"
)

// A skip is sound only for edits that keep every signature; anything else runs Gazelle.
func TestRecordSkipsOnlyEditsThatKeepTheirSignature(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(write func(rel, text string))
		skip bool
	}{
		{"body edit", func(w func(string, string)) { w("app/a.ts", "import './b';\nexport const a = 2;\n") }, true},
		{"new import", func(w func(string, string)) { w("app/a.ts", "import './b';\nimport './c';\nexport const a = 1;\n") }, false},
		{"new file", func(w func(string, string)) { w("app/c.ts", "export {};\n") }, false},
		{"BUILD edit", func(w func(string, string)) { w("app/BUILD.bazel", "# edited\n") }, false},
		{"config edit", func(w func(string, string)) { w("app/vitest.config.ts", "export default { test: {} };\n") }, false},
	} {
		root := t.TempDir()
		write := func(rel, text string) {
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("app/a.ts", "import './b';\nexport const a = 1;\n")
		write("app/b.ts", "export {};\n")
		write("app/BUILD.bazel", "")
		write("app/vitest.config.ts", "export default {};\n")
		opts := Options{Root: root, ByContent: func(rel string) bool { return path.Base(rel) == "BUILD.bazel" }}
		entries, err := Entries(opts)
		if err != nil {
			t.Fatal(err)
		}
		record := NewRecord(root, entries)
		tc.edit(write)
		now, err := Entries(opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := record.Unchanged(root, now); got != tc.skip {
			t.Errorf("%s: skip = %t, want %t", tc.name, got, tc.skip)
		}
	}
}
