// Command quickstart scaffolds a minimal rules_typescript consumer workspace.
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A copy of the ruleset's own MODULE.bazel, so the versions the scaffold pins
// are read from the source of truth instead of restated here.
//
//go:embed module_bazel.txt
var moduleSnapshot embed.FS

// The floor the ruleset supports, and the version its own CI runs. They are
// not the same number: 9.x is the supported range, and only the pin is tested.
const (
	minBazelVersion    = "9.0.0"
	pinnedBazelVersion = "9.3.1"
)

type file struct {
	path string
	body string
	desc string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quickstart: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	dir := fs.String("dir", "my_project", "directory to scaffold, relative to the directory you ran bazel from")
	rulesPath := fs.String("rules-path", "", "path to a local rules_typescript checkout; adds a local_path_override instead of using the registry version")
	bazelVersion := fs.String("bazel-version", pinnedBazelVersion, "value written to .bazelversion (the version rules_typescript pins; 9.0.0+ is supported)")
	force := fs.Bool("force", false, "overwrite files that already exist")
	dryRun := fs.Bool("dry-run", false, "list the files that would be written without writing them")
	fs.Usage = func() {
		fmt.Println(`bazel run //tools/quickstart -- [flags]

Scaffolds a minimal TypeScript workspace that depends on rules_typescript:
a root BUILD.bazel with a Gazelle target, a src/lib library, and a src/app
that imports it. Bazelisk (or Bazel ` + minBazelVersion + `+) is the only prerequisite —
Rust, Node.js, the ruleset's Go tools and every npm package are fetched by Bazel
on first build, and a Go SDK under bazel run //:gazelle.

Flags:`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (see --help)", fs.Arg(0))
	}

	root, err := targetDir(*dir)
	if err != nil {
		return err
	}
	rulesVersion, err := snapshotVersion(`(?m)^module\(\n(?:.*\n)*?\s*version = "([^"]+)"`)
	if err != nil {
		return fmt.Errorf("reading the rules_typescript version: %w", err)
	}
	gazelleVersion, err := snapshotVersion(`bazel_dep\(name = "gazelle", version = "([^"]+)"\)`)
	if err != nil {
		return fmt.Errorf("reading the gazelle version: %w", err)
	}

	override := ""
	if *rulesPath != "" {
		abs, err := filepath.Abs(*rulesPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(abs, "MODULE.bazel")); err != nil {
			return fmt.Errorf("--rules-path %s is not a rules_typescript checkout (no MODULE.bazel)", abs)
		}
		override = fmt.Sprintf("\n# Local development: build against a checkout instead of the registry.\nlocal_path_override(\n    module_name = \"rules_typescript\",\n    path = %q,\n)\n", abs)
	}

	files := scaffold(filepath.Base(root), rulesVersion, gazelleVersion, *bazelVersion, override)

	if *dryRun {
		fmt.Printf("Would create %s with %d files:\n", root, len(files))
		for _, f := range files {
			fmt.Printf("  %-24s %s\n", f.path, f.desc)
		}
		return nil
	}

	if !*force {
		var clashes []string
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(root, f.path)); err == nil {
				clashes = append(clashes, f.path)
			}
		}
		if len(clashes) > 0 {
			sort.Strings(clashes)
			return fmt.Errorf("%s already contains %s.\nDid you mean to pass --force, or --dir with a fresh directory name?",
				root, strings.Join(clashes, ", "))
		}
	}

	fmt.Printf("Creating %s\n", root)
	for _, f := range files {
		full := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(f.body), 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote %-24s %s\n", f.path, f.desc)
	}

	rel := root
	if wd := os.Getenv("BUILD_WORKING_DIRECTORY"); wd != "" {
		if r, err := filepath.Rel(wd, root); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	fmt.Printf(`
Next:

  cd %s
  bazel build //...          # compiles and type-checks; fetches toolchains on first run
  bazel run //:gazelle       # regenerates BUILD files from the .ts sources

The only prerequisite is Bazelisk (or Bazel %s+). Rust, Node.js, the ruleset's
Go tools and the npm packages are all fetched by Bazel; a Go SDK under
bazel run //:gazelle.
`, rel, minBazelVersion)
	return nil
}

func targetDir(dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	base := os.Getenv("BUILD_WORKING_DIRECTORY")
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = wd
	}
	return filepath.Join(base, dir), nil
}

func snapshotVersion(pattern string) (string, error) {
	body, err := moduleSnapshot.ReadFile("module_bazel.txt")
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(pattern).FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("no match for %s in the embedded MODULE.bazel", pattern)
	}
	return string(m[1]), nil
}

func scaffold(name, rulesVersion, gazelleVersion, bazelVersion, override string) []file {
	return []file{
		{".bazelversion", bazelVersion + "\n", "Bazel version bazelisk downloads"},
		{".bazelrc", `# Type-check on every build, so tsc errors fail bazel build the way they
# fail go build. Drop this line for faster single-target iteration.
build --output_groups=+_validation

test --test_output=errors
`, "type-check on every build"},
		{"MODULE.bazel", fmt.Sprintf(`"""Minimal rules_typescript consumer, generated by //tools/quickstart."""

module(
    name = %q,
    version = "0.0.0",
)

bazel_dep(name = "rules_typescript", version = %q)
bazel_dep(name = "gazelle", version = %q)

# Toolchain registration is the consumer's responsibility, as in rules_go.
register_toolchains("@rules_typescript//ts/toolchain:all")
%s`, name, rulesVersion, gazelleVersion, override), "module deps and toolchains"},
		{"BUILD.bazel", `load("@gazelle//:def.bzl", "gazelle")

# bazel run //:gazelle — regenerate every BUILD file from the .ts sources.
gazelle(
    name = "gazelle",
    gazelle = "@rules_typescript//gazelle:gazelle_typescript",
    tags = ["manual"],
)
`, "the Gazelle entry point"},
		{"src/BUILD.bazel", "# Package boundary, so //src/... resolves.\n", "keeps src/ a package"},
		{"src/lib/math.ts", `export function add(a: number, b: number): number {
  return a + b;
}

export function multiply(a: number, b: number): number {
  return a * b;
}
`, "library sources"},
		{"src/lib/index.ts", "export { add, multiply } from \"./math\";\n", "barrel export"},
		{"src/lib/BUILD.bazel", `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "lib",
    srcs = [
        "index.ts",
        "math.ts",
    ],
    visibility = ["//visibility:public"],
)
`, "ts_compile for the library"},
		{"src/app/main.ts", `import { add, multiply } from "../lib/index";

const sum: number = add(2, 3);
const product: number = multiply(4, 5);

console.log("sum:", sum, "product:", product);
`, "app entry point"},
		{"src/app/BUILD.bazel", `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "app",
    srcs = ["main.ts"],
    visibility = ["//visibility:public"],
    deps = ["//src/lib"],
)
`, "ts_compile depending on //src/lib"},
	}
}
