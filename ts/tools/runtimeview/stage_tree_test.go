package runtimeview

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagedTree(t *testing.T, root, source string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			tree[filepath.ToSlash(relative)] = "dir"
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			tree[filepath.ToSlash(relative)] = "link " + strings.Replace(filepath.ToSlash(target), filepath.ToSlash(source), "$SOURCE", 1)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			tree[filepath.ToSlash(relative)] = fmt.Sprintf("file %04o %q", info.Mode().Perm(), data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestStageLeavesTheTreeLinkingThenReplacingEachModuleLeft(t *testing.T) {
	source := t.TempDir()
	writeInput(t, source, "a.js", "a")
	if err := os.WriteFile(filepath.Join(source, "b.js"), []byte("b"), 0o640); err != nil {
		t.Fatal(err)
	}
	writeInput(t, source, "lib/x.js", "x")
	writeInput(t, source, "pkg/f.js", "f")
	probe := filepath.Join(source, "probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	empty := fmt.Sprintf("file %04o %q", info.Mode().Perm(), "")
	modules := []string{"_main/m/a.js", "_main/m/b.js", "_main/m/empty.js", "_main/m/rel.js"}
	entries := map[string]string{
		"_main/m/a.js":          filepath.Join(source, "a.js"),
		"_main/m/b.js":          filepath.Join(source, "b.js"),
		"_main/m/empty.js":      "",
		"_main/m/rel.js":        "a.js",
		"_main/alias.js":        filepath.Join(source, "a.js"),
		"_main/plain.js":        filepath.Join(source, "lib/x.js"),
		"_main/data":            "",
		"_main/deep/n/o/p.js":   filepath.Join(source, "b.js"),
		"_main/node_modules/p":  "../pkg",
		"_main/pkg":             filepath.Join(source, "pkg"),
		"_main/pkg/f.js":        filepath.Join(source, "pkg/f.js"),
		"_main/nested/z/z.js":   filepath.Join(source, "lib/x.js"),
		"_main/nested/z/zz.txt": "",
	}
	want := map[string]string{
		"_main":                 "dir",
		"_main/m":               "dir",
		"_main/m/a.js":          `file 0751 "a"`,
		"_main/m/b.js":          `file 0640 "b"`,
		"_main/m/empty.js":      empty,
		"_main/m/rel.js":        `file 0751 "a"`,
		"_main/alias.js":        "link m/a.js",
		"_main/plain.js":        "link $SOURCE/lib/x.js",
		"_main/data":            empty,
		"_main/deep":            "dir",
		"_main/deep/n":          "dir",
		"_main/deep/n/o":        "dir",
		"_main/deep/n/o/p.js":   "link ../../../m/b.js",
		"_main/node_modules":    "dir",
		"_main/node_modules/p":  "link ../pkg",
		"_main/pkg":             "link $SOURCE/pkg",
		"_main/nested":          "dir",
		"_main/nested/z":        "dir",
		"_main/nested/z/z.js":   "link $SOURCE/lib/x.js",
		"_main/nested/z/zz.txt": empty,
	}
	for _, test := range []struct {
		name    string
		entries map[string]string
		before  map[string]string
		want    map[string]string
	}{
		{name: "fresh root", entries: entries, want: want},
		{
			name:    "root with earlier entries",
			entries: entries,
			before:  map[string]string{"_main/m/a.js": "stale", "_main/plain.js": "stale", "stale.txt": "stale"},
			want:    map[string]string{"stale.txt": "file 0751 \"stale\""},
		},
		{
			// validateEntries stats this child's target through the staged module path.
			name:    "relative entry beneath a directory entry",
			entries: map[string]string{"_main/src": source, "_main/src/a.js": "../m/a.js"},
			want:    map[string]string{"_main/src": "link $SOURCE"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for name, data := range test.before {
				writeInput(t, root, name, data)
			}
			staged := maps.Clone(entries)
			maps.Copy(staged, test.entries)
			if err := Stage(root, staged, modules); err != nil {
				t.Fatal(err)
			}
			expected := maps.Clone(want)
			maps.Copy(expected, test.want)
			got := stagedTree(t, root, source)
			for name := range expected {
				if got[name] != expected[name] {
					t.Errorf("%s = %q, want %q", name, got[name], expected[name])
				}
			}
			for name := range got {
				if _, ok := expected[name]; !ok {
					t.Errorf("unexpected %s = %q", name, got[name])
				}
			}
		})
	}
}
