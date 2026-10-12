package attrs_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// Two modules of one directory pair, a directory whose modules run from two
// directories, a sibling whose runtime does not line up, a source deeper than its
// runtime and a module outside the workspace.
const moduleReferencesFixture = `[
  ["lib/a/x.ts", "_main/lib/a/x.ts", "_main/lib/dist/a/x.js", "_main/lib/dist/a/x.js", ".ts"],
  ["lib/a/y.tsx", "_main/lib/a/y.tsx", "_main/lib/dist/a/y.js", "_main/lib/dist/a/y.jsx", ".tsx"],
  ["lib/c/u.ts", "_main/lib/c/u.ts", "_main/lib/dist/c/u.js", "_main/lib/dist/c/u.js", ".ts"],
  ["lib/c/v.ts", "_main/lib/c/v.ts", "_main/lib/gen/c/v.js", "_main/lib/gen/c/v.js", ".ts"],
  ["lib/b/z.ts", "_main/lib/b/z.ts", "_main/lib/b/z.js", "_main/lib/b/z.js", ".ts"],
  ["deep/p/q/w.mts", "_main/deep/p/q/w.mts", "_main/w.mjs", "_main/w.mjs", ".mts"],
  ["../ext/m.ts", "ext/m.ts", "ext/out/m.js", "ext/out/m.js", ".ts"]
]`

func TestModuleLookupsKeepEveryAncestorAndConflictWhenModulesShareADirectory(t *testing.T) {
	tree := verify.New(t)
	node := tree.File("ts/toolchain/node_resolved/node")
	config := tree.File("tests/vitest/attrs/_attrs_test.vitest/config.mjs")
	if !node.Exists() || !config.Exists() {
		t.FailNow()
	}
	text := config.Text()
	start := strings.Index(text, "const moduleReferences = MODULE_REFERENCES.map")
	end := strings.Index(text, "const moduleReference = ")
	if start < 0 || end < start {
		t.Fatalf("%s holds no module lookup block", config.Name())
	}

	probe := filepath.Join(t.TempDir(), "probe.mjs")
	script := strings.Join([]string{
		"import { dirname, relative, resolve } from 'node:path';",
		"const WORKSPACE_DIR = '/r/_main';",
		"const RUNFILES_ROOT = '/r';",
		"const MODULE_REFERENCES = " + moduleReferencesFixture + ";",
		text[start:end],
		"process.stdout.write(JSON.stringify({ lookups: [...moduleLookups], exact: [...exactModuleReferences] }));",
	}, "\n")
	if err := os.WriteFile(probe, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node.Abs(), probe).Output()
	if err != nil {
		t.Fatalf("evaluating the module lookups: %v", err)
	}
	var got struct {
		Lookups [][2]*string
		Exact   [][2]string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}

	ambiguous := ""
	want := [][2]string{
		{"/r/_main/lib/a/x", "/r/_main/lib/dist/a/x"},
		{"/r/_main/lib/a", "/r/_main/lib/dist/a"},
		{"/r/_main/lib", ambiguous},
		{"/r/_main", ambiguous},
		{"/r/_main/lib/a/y", "/r/_main/lib/dist/a/y"},
		{"/r/_main/lib/c/u", "/r/_main/lib/dist/c/u"},
		{"/r/_main/lib/c", ambiguous},
		{"/r/_main/lib/c/v", "/r/_main/lib/gen/c/v"},
		{"/r/_main/lib/b/z", "/r/_main/lib/b/z"},
		{"/r/_main/lib/b", "/r/_main/lib/b"},
		{"/r/_main/deep/p/q/w", "/r/_main/w"},
		{"/r/_main/deep/p/q", "/r/_main"},
		{"/r/_main/deep/p", "/r"},
		{"/r/_main/deep", "/"},
		{"/r/ext/m", "/r/ext/out/m"},
		{"/r/ext", "/r/ext/out"},
	}
	var lookups [][2]string
	for _, entry := range got.Lookups {
		candidate := ambiguous
		if entry[1] != nil {
			candidate = *entry[1]
		}
		lookups = append(lookups, [2]string{*entry[0], candidate})
	}
	if !reflect.DeepEqual(lookups, want) {
		t.Errorf("moduleLookups (empty = ambiguous):\n got %q\nwant %q", lookups, want)
	}
	wantExact := [][2]string{
		{"/r/_main/lib/a/x.ts", "/r/_main/lib/dist/a/x.js"},
		{"/r/_main/lib/a/y.tsx", "/r/_main/lib/dist/a/y.js"},
		{"/r/_main/lib/c/u.ts", "/r/_main/lib/dist/c/u.js"},
		{"/r/_main/lib/c/v.ts", "/r/_main/lib/gen/c/v.js"},
		{"/r/_main/lib/b/z.ts", "/r/_main/lib/b/z.js"},
		{"/r/_main/deep/p/q/w.mts", "/r/_main/w.mjs"},
		{"/r/ext/m.ts", "/r/ext/out/m.js"},
	}
	if !reflect.DeepEqual(got.Exact, wantExact) {
		t.Errorf("exactModuleReferences:\n got %q\nwant %q", got.Exact, wantExact)
	}
}
