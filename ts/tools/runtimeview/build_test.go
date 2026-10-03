package runtimeview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInput(t *testing.T, root, name, data string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o751); err != nil {
		t.Fatal(err)
	}
	return path
}

func sameAuthority(t *testing.T, first, second string, same bool) {
	t.Helper()
	a, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, b) != same {
		t.Fatalf("authority equality for %s and %s = %t, want %t", first, second, !same, same)
	}
}

func TestBuildRetainsCanonicalAliasesWithoutInferringRegularFileProvenance(t *testing.T) {
	inputs, output := t.TempDir(), t.TempDir()
	source := writeInput(t, inputs, "source/main.js", "export const singleton = {};\n")
	alias := writeInput(t, inputs, "generated/alias.js", "transported alias bytes")
	twin := writeInput(t, inputs, "equal/main.js", "export const singleton = {};\n")
	regular := filepath.Join(inputs, "regular.js")
	if err := os.Symlink(source, regular); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(output, "view")
	canonical := filepath.Join(root, "_main/main.js")
	spec := Spec{Root: root, Modules: []string{"_main/main.js"}, Inputs: []Input{
		{Path: source, Output: filepath.Join(output, "files/source/main.js"), Kind: "alias", Target: canonical},
		{Path: alias, Output: filepath.Join(output, "files/generated/alias.js"), Kind: "alias", Target: canonical},
		{Path: twin, Output: filepath.Join(output, "files/equal/main.js"), Kind: "file"},
		{Path: regular, Output: filepath.Join(output, "files/regular.js"), Kind: "file"},
	}, Entries: map[string]string{"_main/main.js": source, "_main/alias.js": alias, "_main/data.js": source, "_main/equal.js": twin, "_main/regular.js": regular}}
	if err := Build(spec); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alias.js", "data.js"} {
		sameAuthority(t, canonical, filepath.Join(root, "_main", name), true)
	}
	for _, name := range []string{"equal.js", "regular.js"} {
		sameAuthority(t, canonical, filepath.Join(root, "_main", name), false)
	}
	info, err := os.Lstat(spec.Inputs[3].Output)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("regular File transport became inferred symlink: %v, %v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "_main/alias.js")); err != nil || string(got) != "export const singleton = {};\n" {
		t.Fatalf("canonical fact lost to alias transport bytes: %q, %v", got, err)
	}
	if info, err := os.Stat(canonical); err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("module mode changed: %v, %v", info, err)
	}
}

func TestBuildDeclaredLinksAndOpaqueDataSurviveCompleteRelocation(t *testing.T) {
	inputs, construction := t.TempDir(), t.TempDir()
	asset := writeInput(t, inputs, "transport/asset.txt", "opaque asset")
	if err := os.Mkdir(filepath.Join(inputs, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(asset, filepath.Join(inputs, "tree/asset.txt")); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"live.js": "tree/asset.txt", "missing.json": "absent.json", "cycle-a": "cycle-b", "cycle-b": "cycle-a"} {
		if err := os.Symlink(target, filepath.Join(inputs, name)); err != nil {
			t.Fatal(err)
		}
	}
	prefix := filepath.Join(construction, "composed")
	spec := Spec{Root: filepath.Join(prefix, "view"), Entries: map[string]string{}}
	for _, name := range []string{"tree", "live.js", "missing.json", "cycle-a", "cycle-b"} {
		kind := "symlink"
		if name == "tree" {
			kind = "directory"
		}
		source := filepath.Join(inputs, name)
		spec.Inputs = append(spec.Inputs, Input{Path: source, Output: filepath.Join(prefix, "files", name), Kind: kind})
		spec.Entries["_main/"+name] = source
	}
	if err := Build(spec); err != nil {
		t.Fatal(err)
	}
	for _, input := range spec.Inputs {
		if _, err := os.Lstat(input.Output); err != nil {
			t.Fatalf("declared output absent: %s: %v", input.Output, err)
		}
	}
	for name := range spec.Entries {
		if _, err := os.Lstat(filepath.Join(spec.Root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("declared view output absent: %s: %v", name, err)
		}
	}
	relocated := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(prefix, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(inputs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(prefix); !os.IsNotExist(err) {
		t.Fatalf("construction prefix remains available: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(relocated, "files/tree/asset.txt")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("transported directory leaf was not materialized as a regular file: %v, %v", info, err)
	}
	root := filepath.Join(relocated, "view")
	for _, name := range []string{"tree/asset.txt", "live.js"} {
		if data, err := os.ReadFile(filepath.Join(root, "_main", name)); err != nil || string(data) != "opaque asset" {
			t.Fatalf("relocated %s lost data: %q, %v", name, data, err)
		}
	}
	sameAuthority(t, filepath.Join(root, "_main/live.js"), filepath.Join(root, "_main/tree/asset.txt"), true)
	for name, target := range map[string]string{"missing.json": "absent.json", "cycle-a": "cycle-b", "cycle-b": "cycle-a"} {
		if got, err := os.Readlink(filepath.Join(relocated, "files", name)); err != nil || got != target {
			t.Fatalf("unrelated declared link changed: %q, %v", got, err)
		}
	}
}

func TestBuildCreatesDemandedLinkEvenWhenAncestorAlreadyUsesSameStore(t *testing.T) {
	inputs, output := t.TempDir(), t.TempDir()
	module := writeInput(t, inputs, "app/main.js", "export {};\n")
	writeInput(t, inputs, "store/package.json", `{"name":"pkg"}`)
	store := filepath.Join(inputs, "store")
	root := filepath.Join(output, "view")
	spec := Spec{Root: root, Modules: []string{"_main/app/main.js"}, Inputs: []Input{
		{Path: module, Output: filepath.Join(output, "files/app/main.js"), Kind: "alias", Target: filepath.Join(root, "_main/app/main.js")},
		{Path: store, Output: filepath.Join(output, "files/store"), Kind: "directory"},
	}, Entries: map[string]string{"_main/app/main.js": module, "_main/store/pkg": store, "_main/node_modules/pkg": store}, NpmContexts: []NpmContext{{Module: "_main/app/main.js", Source: "app/main.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}}}}
	if err := Build(spec); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "_main/app/node_modules/pkg")
	if _, err := os.Lstat(local); err != nil {
		t.Fatalf("declared npm link absent when ancestor matches: %v", err)
	}
	sameAuthority(t, local, filepath.Join(root, "_main/store/pkg"), true)
	sameAuthority(t, local, filepath.Join(root, "_main/node_modules/pkg"), true)
}

func TestBuildCannotReplaceOpaqueChildWithEqualBytesFromAnotherFile(t *testing.T) {
	inputs, output := t.TempDir(), t.TempDir()
	writeInput(t, inputs, "tree/child.json", "same bytes")
	other := writeInput(t, inputs, "other.json", "same bytes")
	tree := filepath.Join(inputs, "tree")
	spec := Spec{Root: filepath.Join(output, "view"), Inputs: []Input{{Path: tree, Output: filepath.Join(output, "files/tree"), Kind: "directory"}, {Path: other, Output: filepath.Join(output, "files/other.json"), Kind: "file"}}, Entries: map[string]string{"_main/tree": tree, "_main/tree/child.json": other}}
	if err := Build(spec); err == nil || !strings.Contains(err.Error(), "conflicts with directory entry") {
		t.Fatalf("different equal-byte authority admitted: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(tree, "child.json")); err != nil || string(got) != "same bytes" {
		t.Fatalf("rejection changed input: %q, %v", got, err)
	}
}

func TestBuildRejectsUnavailableOrEscapingRuntimeInputs(t *testing.T) {
	for _, failure := range []string{"absent entry", "missing input", "opaque parent", "escaping path", "optional package"} {
		t.Run(failure, func(t *testing.T) {
			inputs, output := t.TempDir(), t.TempDir()
			source := writeInput(t, inputs, "app/main.js", "export {};\n")
			root := filepath.Join(output, "view")
			module := "_main/app/main.js"
			spec := Spec{Root: root, Modules: []string{module}, Entries: map[string]string{module: source}, Inputs: []Input{{Path: source, Output: filepath.Join(output, "files/app/main.js"), Kind: "alias", Target: filepath.Join(root, module)}}}
			want := "no individual runfiles entry"
			switch failure {
			case "absent entry":
				delete(spec.Entries, module)
			case "missing input":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				want = "no such file"
			case "opaque parent":
				parent := filepath.Dir(source)
				spec.Entries["_main/app"] = parent
				spec.Inputs = append(spec.Inputs, Input{Path: parent, Output: filepath.Join(output, "files/tree"), Kind: "directory"})
				want = "beneath directory entry"
			case "optional package":
				spec.OptionalDeps = []PackageLink{{Name: "missing-manifest"}}
				want = "non-normalized"
			case "escaping path":
				delete(spec.Entries, module)
				spec.Entries["../outside.js"] = source
				spec.Modules = []string{"../outside.js"}
				want = "non-normalized"
			}
			if err := Build(spec); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("invalid module accepted: %v, want %q", err, want)
			}
		})
	}
}

func TestManifestEntryReadsBothLineShapes(t *testing.T) {
	const link, tree = "_main/node_modules/zod", "../.pnpm/zod@3/node_modules/zod"
	const store = "_main/node_modules/.pnpm/zod@3/node_modules/zod"
	for _, tc := range []struct{ line, rlocation, target string }{
		{link + " " + tree, link, tree},
		{store + " /abs/tree", store, "/abs/tree"},
		{` _main/a\sb /abs/a\sb\bc`, "_main/a b", `/abs/a b\c`},
	} {
		rlocation, target, ok := ManifestEntry(tc.line)
		if !ok || rlocation != tc.rlocation || target != tc.target {
			t.Errorf("ManifestEntry(%q) = %q, %q, %v; want %q, %q",
				tc.line, rlocation, target, ok, tc.rlocation, tc.target)
		}
	}
	if _, _, ok := ManifestEntry(""); ok {
		t.Error("an empty line is no entry")
	}
}
