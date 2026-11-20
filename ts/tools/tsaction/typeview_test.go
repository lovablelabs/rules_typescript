package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestTypeViewKeepsWhatResolutionReads(t *testing.T) {
	in := t.TempDir()
	for _, rel := range []string{
		"package.json",
		"index.js", "index.d.ts", "index.js.map",
		"esm/index.mjs", "esm/index.d.mts",
		"cjs/index.cjs", "cjs/index.d.ts",
		"cts/index.cjs", "cts/index.d.cts",
		"untyped.js", "untyped.mjs", "untyped.cjs",
		"src/a.ts", "src/b.tsx",
		"data/locale.json",
		"styles.css", "README.md",
		"assets/logo.png",
	} {
		at := filepath.Join(in, rel)
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("src", filepath.Join(in, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.d.ts", filepath.Join(in, "alias.d.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.js.map", filepath.Join(in, "alias.js.map")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "view")
	if err := typeView([]string{"-in=" + in, "-out=" + out}); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			rel += " -> " + target
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{
		"alias.d.ts",
		"cjs/index.cjs",
		"cjs/index.d.ts",
		"cts/index.d.cts",
		"data/locale.json",
		"esm/index.d.mts",
		"index.d.ts",
		"linked -> src",
		"package.json",
		"src/a.ts",
		"src/b.tsx",
		"untyped.cjs",
		"untyped.js",
		"untyped.mjs",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("view holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
