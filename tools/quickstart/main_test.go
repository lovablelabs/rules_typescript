package main

import (
	"strings"
	"testing"
)

func TestSnapshotVersionsComeFromTheRealModuleFile(t *testing.T) {
	rules, err := snapshotVersion(`(?m)^module\(\n(?:.*\n)*?\s*version = "([^"]+)"`)
	if err != nil {
		t.Fatal(err)
	}
	gazelle, err := snapshotVersion(`bazel_dep\(name = "gazelle", version = "([^"]+)"\)`)
	if err != nil {
		t.Fatal(err)
	}
	body, err := moduleSnapshot.ReadFile("module_bazel.txt")
	if err != nil {
		t.Fatal(err)
	}
	// The module version must not be read off one of the bazel_dep lines.
	if !strings.Contains(string(body), "module(\n    name = \"rules_typescript\",\n    version = \""+rules+"\"") {
		t.Errorf("module version %q is not the one inside module()", rules)
	}
	if !strings.Contains(string(body), `bazel_dep(name = "gazelle", version = "`+gazelle+`")`) {
		t.Errorf("gazelle version %q not found as a bazel_dep", gazelle)
	}
}

func TestScaffoldShape(t *testing.T) {
	files := scaffold("my_project", "0.1.0", "0.51.3", "9.0.0", "")
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.path] {
			t.Errorf("duplicate path %s", f.path)
		}
		seen[f.path] = true
		if f.desc == "" {
			t.Errorf("%s has no description to print", f.path)
		}
	}
	for _, want := range []string{"MODULE.bazel", "BUILD.bazel", "src/lib/BUILD.bazel", "src/app/BUILD.bazel", ".bazelrc"} {
		if !seen[want] {
			t.Errorf("scaffold is missing %s", want)
		}
	}

	module := body(t, files, "MODULE.bazel")
	for _, want := range []string{
		`bazel_dep(name = "rules_typescript", version = "0.1.0")`,
		`bazel_dep(name = "gazelle", version = "0.51.3")`,
		`register_toolchains("@rules_typescript//ts/toolchain:all")`,
	} {
		if !strings.Contains(module, want) {
			t.Errorf("MODULE.bazel is missing %q:\n%s", want, module)
		}
	}
	if strings.Contains(module, "local_path_override") {
		t.Error("an empty --rules-path must not emit a local_path_override")
	}

	withOverride := body(t, scaffold("p", "0.1.0", "0.51.3", "9.0.0", "\nlocal_path_override(\n)\n"), "MODULE.bazel")
	if !strings.Contains(withOverride, "local_path_override") {
		t.Error("--rules-path must emit a local_path_override")
	}
}

func body(t *testing.T, files []file, path string) string {
	t.Helper()
	for _, f := range files {
		if f.path == path {
			return f.body
		}
	}
	t.Fatalf("%s not in the scaffold", path)
	return ""
}
