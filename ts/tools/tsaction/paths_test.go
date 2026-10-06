package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

func readPaths(t *testing.T, path string) pathsFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file pathsFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestWritePaths_TheChainsMapFromItsWritersDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "base/tsconfig.base.json", baseConfig)
	writeFile(t, "pkg/test/tsconfig.json",
		`{"extends": "../../base/tsconfig.base.json"}`)
	err := writePaths([]string{
		"-tsconfig=pkg/test/tsconfig.json", "-package=pkg/test",
		"-bin_dir=" + binDir, "-out=out.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readPaths(t, "out.json")
	want := pathsFile{
		Dir:   "../../base",
		Paths: &tsconfig.Paths{{Key: "@app/*", Values: []string{"src/*"}}, {Key: "#lib", Values: []string{"lib/index.ts"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths file = %+v, want %+v", got, want)
	}
}

func TestWritePaths_ALeafReplacesTheMapWhole(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "base/tsconfig.base.json", baseConfig)
	writeFile(t, "pkg/tsconfig.json", `{
  "extends": "../base/tsconfig.base.json",
  "compilerOptions": { "paths": { "@pkg": ["./lib/pkg.ts"] } }
}`)
	err := writePaths([]string{
		"-tsconfig=pkg/tsconfig.json", "-package=pkg", "-out=out.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readPaths(t, "out.json")
	want := pathsFile{
		Dir:   ".",
		Paths: &tsconfig.Paths{{Key: "@pkg", Values: []string{"./lib/pkg.ts"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths file = %+v, want %+v", got, want)
	}
}

func TestWritePaths_NoPathsIsAnEmptyMap(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "pkg/tsconfig.json", `{"compilerOptions": {"strict": true}}`)
	err := writePaths([]string{
		"-tsconfig=pkg/tsconfig.json", "-package=pkg", "-out=out.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readPaths(t, "out.json")
	want := pathsFile{Paths: &tsconfig.Paths{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths file = %+v, want %+v", got, want)
	}
}

func TestWritePaths_AGeneratedTsconfigIsAtItsPackage(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(binDir, "pkg/tsconfig.json"),
		`{"compilerOptions": {"paths": {"@x/*": ["./x/*"]}}}`)
	err := writePaths([]string{
		"-tsconfig=" + binDir + "/pkg/tsconfig.json", "-package=pkg",
		"-bin_dir=" + binDir, "-out=out.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readPaths(t, "out.json").Dir; got != "." {
		t.Errorf("dir = %q, want %q", got, ".")
	}
}
