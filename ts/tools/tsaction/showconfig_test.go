package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Captures of `tsgo --showConfig -p` (7.0.2), paths relative to the written
// config; a base sets paths, jsx and typeRoots, a leaf types or nothing.
const baseConfig = `{
  // A comment: tsconfig.json is JSONC.
  "compilerOptions": {
    "target": "es2017",
    "jsx": "react-jsx",
    "jsxImportSource": "preact",
    "paths": { "@app/*": ["src/*"], "#lib": ["lib/index.ts"] },
    "strict": true,
    "typeRoots": ["./typings"],
  }
}
`

const chainLeaf = `{
  "extends": "../base/tsconfig.base.json",
  "compilerOptions": {
    "types": ["./globals.d.ts", "./generated.d.ts", "node", "@cloudflare/workers-types"],
    "lib": ["es2022"]
  },
  "include": ["src"]
}
`

const filesLeaf = `{
  "extends": "../base/tsconfig.base.json",
  "files": ["src/a.ts", "globals.d.ts"]
}
`

const patternLeaf = `{
  "extends": "../base/tsconfig.base.json",
  "include": ["./src/**/*", "globals.d.ts"],
  "exclude": ["src/**/*.test.ts"]
}
`

// A base in another directory sets the roots; the leaf sets none of its own.
const rootsBase = `{
  "extends": "./tsconfig.base.json",
  "include": ["../pkg/src"],
  "exclude": ["**/*.test.ts"]
}
`

const inheritedRootsLeaf = `{
  "extends": "../base/tsconfig.roots.json",
  "compilerOptions": { "types": ["./globals.d.ts", "./generated.d.ts"] }
}
`

const noTypesLeaf = `{
  "extends": "../base/tsconfig.base.json",
  "compilerOptions": { "target": "esnext", "jsx": "preserve", "module": "nodenext" }
}
`

const noRootsLeaf = `{
  "compilerOptions": {
    "target": "esnext", "jsx": "preserve", "module": "nodenext"
  }
}
`

// The shape of a package annotated for isolated declarations: the chain sets
// declaration, isolatedDeclarations and declarationMap itself.
const isolatedLeaf = `{
  "extends": "../base/tsconfig.base.json",
  "compilerOptions": {
    "module": "esnext",
    "declaration": true,
    "isolatedDeclarations": true,
    "declarationMap": true
  },
  "include": ["src"]
}
`

const binDir = "bazel-out/k8-fastbuild/bin"

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeTool writes an executable that records its arguments, then runs script.
func fakeTool(t *testing.T, root, name, script string) (bin, argv string) {
	t.Helper()
	argv = filepath.Join(root, name+".argv")
	bin = filepath.Join(root, name)
	writeFile(t, bin, "#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argv+"\n"+script)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argv
}

func recordedArgs(t *testing.T, argv string) []string {
	t.Helper()
	data, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

type execroot struct {
	tsgo string
	argv string
}

// newExecroot lays the action's sandbox out under a temp dir and makes it the
// working directory, with a tsgo whose --showConfig prints showConfig.
func newExecroot(t *testing.T, leaf, showConfig string) *execroot {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"base/tsconfig.base.json":                           baseConfig,
		"base/tsconfig.roots.json":                          rootsBase,
		"pkg/tsconfig.json":                                 leaf,
		"pkg/src/a.ts":                                      "export const a = 1;\n",
		"pkg/globals.d.ts":                                  "declare const G: number;\n",
		binDir + "/pkg/generated.d.ts":                      "declare const generated: string;\n",
		binDir + "/pkg/node_modules/react/index.d.ts":       "export {};\n",
		binDir + "/pkg/node_modules/@types/node/index.d.ts": "declare const process: unknown;\n",
		binDir + "/pkg/pkg.tsconfig_baseline.json":          `{"compilerOptions": {"strict": true}}`,
		"showconfig.json":                                   showConfig,
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(root, rel), body)
	}
	tsgo, argv := fakeTool(t, root, "tsgo", "cat "+filepath.Join(root, "showconfig.json")+"\n")
	t.Chdir(root)
	return &execroot{tsgo: tsgo, argv: argv}
}

func (e *execroot) tsconfigArgs(extra ...string) []string {
	args := []string{
		"-tsgo=" + e.tsgo,
		"-tsconfig=pkg/tsconfig.json",
		"-baseline=" + binDir + "/pkg/pkg.tsconfig_baseline.json",
		"-out=" + binDir + "/pkg/pkg.tsconfig.json",
		"-options=" + binDir + "/pkg/pkg.options.json",
		"-bin_dir=" + binDir,
	}
	args = append(args, extra...)
	return append(args, "pkg/src/a.ts", "pkg/globals.d.ts")
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, data)
	}
	return out
}

// assertJSON compares two documents structurally: both are re-encoded with
// sorted keys, so the expectation reads as the JSON it stands for.
func assertJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatalf("the expectation for %s is not JSON: %v", what, err)
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(decoded, "", "  ")
	if string(g) != string(w) {
		t.Errorf("%s:\n%s\nwant:\n%s", what, g, w)
	}
}

func mustWriteTsconfig(t *testing.T, args []string) {
	t.Helper()
	if err := writeTsconfig(args); err != nil {
		t.Fatalf("tsaction tsconfig: %v", err)
	}
}

// tsgo prints the merged compilerOptions with every enum as its lowercase
// name, so target and jsx reach oxc as the strings tsgo printed.
func TestDecodeShowConfig_EnumsAreNames(t *testing.T) {
	capture := readTestdata(t, "showconfig-chain.json")
	got, roots, err := decodeShowConfig([]byte(capture))
	if err != nil {
		t.Fatal(err)
	}
	wantRoots := []string{"../../../../pkg/src/a.ts"}
	if !reflect.DeepEqual(roots, wantRoots) {
		t.Errorf("roots = %q, want %q", roots, wantRoots)
	}
	want := &effectiveOptions{
		compilerOptions: got.compilerOptions,
		Target:          "es2017",
		Jsx:             "react-jsx",
		JsxImportSource: "preact",
		Module:          "es6",
		Types:           &[]string{"./globals.d.ts", "./generated.d.ts", "node", "@cloudflare/workers-types"},
		TypeRoots:       []string{"../base/typings"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeShowConfig = %+v, want %+v", got, want)
	}

	capture = readTestdata(t, "showconfig-no-types.json")
	noTypes, _, err := decodeShowConfig([]byte(capture))
	if err != nil {
		t.Fatal(err)
	}
	if noTypes.Types != nil {
		t.Errorf("Types = %v, want nil: the leaf sets no types key", *noTypes.Types)
	}
	if noTypes.Target != "esnext" || noTypes.Jsx != "preserve" || noTypes.JsxImportSource != "preact" {
		t.Errorf("Target, Jsx, JsxImportSource = %q, %q, %q; want esnext, preserve, preact",
			noTypes.Target, noTypes.Jsx, noTypes.JsxImportSource)
	}
	if want := []string{"../base/typings"}; !reflect.DeepEqual(
		noTypes.TypeRoots, want) {
		t.Errorf("TypeRoots = %v, want %v: the base sets it", noTypes.TypeRoots, want)
	}

	capture = readTestdata(t, "showconfig-no-roots.json")
	noRoots, _, err := decodeShowConfig([]byte(capture))
	if err != nil {
		t.Fatal(err)
	}
	if noRoots.TypeRoots != nil {
		t.Errorf("TypeRoots = %v, want nil: the leaf sets no typeRoots key",
			noRoots.TypeRoots)
	}
}

func TestDecodeShowConfig_DiagnosticsAreNotAConfig(t *testing.T) {
	diagnostic := "error TS5023: Unknown compiler option 'x'.\n"
	_, _, err := decodeShowConfig([]byte(diagnostic))
	if err == nil || !strings.Contains(err.Error(), "TS5023") {
		t.Errorf("decodeShowConfig(diagnostic) = %v, want an error quoting the diagnostic", err)
	}
}

// oxc refuses a top-level await below target es2022; the flag keeps one where
// tsc keeps it, by the chain's module.
func TestOxcOptionsFlags_TopLevelAwaitFollowsTheModule(t *testing.T) {
	kept := oxcOptions{Target: "es2020", Module: "esnext"}
	want := []string{"--target", "es2020", "--top-level-await"}
	if got := kept.flags(); !reflect.DeepEqual(got, want) {
		t.Errorf("flags() = %q, want %q", got, want)
	}
	refused := oxcOptions{Target: "es2020", Module: "es2020"}
	want = []string{"--target", "es2020"}
	if got := refused.flags(); !reflect.DeepEqual(got, want) {
		t.Errorf("flags() = %q, want %q", got, want)
	}
}

// Native showConfig owns inherited options; action paths keep their existing owner.
func TestTsconfigStep_WritesTheChainShapedConfig(t *testing.T) {
	capture := readTestdata(t, "showconfig-chain.json")
	e := newExecroot(t, chainLeaf, capture)

	mustWriteTsconfig(t, e.tsconfigArgs("-types_dep=node"))

	if got, want := recordedArgs(t, e.argv), []string{"--showConfig", "-p", binDir + "/pkg/pkg.tsconfig.json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tsgo ran with %q, want %q", got, want)
	}
	assertJSON(t, "pkg.tsconfig.json", readJSON(t, binDir+"/pkg/pkg.tsconfig.json"), `{
  "compilerOptions": {
    "jsx": "react-jsx",
    "jsxImportSource": "preact",
    "lib": ["es2022"],
    "strict": true,
    "target": "es2017",
    "typeRoots": ["../base/typings"],
    "module": "es6",
    "useDefineForClassFields": false,
    "composite": false,
    "declaration": false,
    "declarationDir": null,
    "declarationMap": false,
    "emitDeclarationOnly": false,
    "incremental": false,
    "paths": {
      "#lib": ["../../../../base/lib/index.ts", "../base/lib/index.ts"],
      "@app/*": ["../../../../base/src/*", "../base/src/*"]
    },
    "rootDir": "../../../..",
    "rootDirs": ["../../../..", ".."],
    "types": ["node", "@cloudflare/workers-types"]
  },
  "include": ["../../../../pkg/src", "../../../../pkg/globals.d.ts",
    "../../../../pkg/generated.d.ts"],
  "exclude": [],
  "references": []
}`)
	assertJSON(t, "pkg.options.json", readJSON(t, binDir+"/pkg/pkg.options.json"),
		`{"target": "es2017", "jsx": "react-jsx", "jsxImportSource": "preact",
		  "module": "es6"}`)
}

// The roots are the chain's own files, include and exclude, each spec from its
// writer's directory; a src no root names joins include, a matched one nothing.
func TestTsconfigStep_RootsAreTheChainsPatterns(t *testing.T) {
	for name, tc := range map[string]struct {
		leaf, capture, files, include, exclude string
	}{
		"leaf": {
			patternLeaf, "showconfig-roots.json",
			`null`,
			`["../../../../pkg/src/**/*", "../../../../pkg/globals.d.ts",
			  "../../../../other/c.ts"]`,
			`["../../../../pkg/src/**/*.test.ts"]`,
		},
		"inherited": {
			inheritedRootsLeaf, "showconfig-chain.json",
			`null`,
			`["../../../../pkg/src", "../../../../pkg/globals.d.ts",
			  "../../../../other/c.ts", "../../../../pkg/generated.d.ts"]`,
			`["../../../../base/**/*.test.ts"]`,
		},
		"files": {
			filesLeaf, "showconfig-roots.json",
			`["../../../../pkg/src/a.ts", "../../../../pkg/globals.d.ts"]`,
			`["../../../../other/c.ts"]`, `[]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newExecroot(t, tc.leaf, readTestdata(t, tc.capture))
			mustWriteTsconfig(t, append(e.tsconfigArgs(), "other/c.ts"))
			config := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
			assertJSON(t, "files", config["files"], tc.files)
			assertJSON(t, "include", config["include"], tc.include)
			assertJSON(t, "exclude", config["exclude"], tc.exclude)
		})
	}
}

// A path-shaped entry is a root file, so the lookup names the file itself.
func TestTypesEntryFile(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeFile(t, "a.d.ts", "")
	writeFile(t, "c.ts", "")
	writeFile(t, "d/index.d.ts", "")
	for in, want := range map[string]string{
		"a.d.ts": "a.d.ts", "c": "c.ts", "d": "d/index.d.ts", "missing": "",
	} {
		if got := typesEntryFile(in, isFile); got != want {
			t.Errorf("typesEntryFile(%q) = %q, want %q", in, got, want)
		}
	}
}

// The first pass hands --showConfig the chain's roots, tsc's default include
// over the project when it names none, and allowJs for a JavaScript src.
func TestTsconfigStep_FirstPassReadsTheChainsRoots(t *testing.T) {
	extends := `"extends": ` +
		`["./pkg.tsconfig_baseline.json", "../../../../pkg/tsconfig.json"]`
	for name, tc := range map[string]struct {
		leaf string
		srcs []string
		want string
	}{
		"files":   {filesLeaf, nil, "{" + extends + "}"},
		"include": {chainLeaf, nil, "{" + extends + "}"},
		"none": {noTypesLeaf, nil,
			"{" + extends + `, "include": ["../../../../pkg/**/*"]}`},
		"javascript": {chainLeaf, []string{"pkg/src/b.js"},
			"{" + extends + `, "compilerOptions": {"allowJs": true}}`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newExecroot(t, tc.leaf, readTestdata(t, "showconfig-roots.json"))
			root, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			firstPass := filepath.Join(root, "first-pass.json")
			e.tsgo, e.argv = fakeTool(t, root, "tsgo",
				"cp \"$3\" "+firstPass+"\ncat "+filepath.Join(root, "showconfig.json")+"\n")

			mustWriteTsconfig(t, append(e.tsconfigArgs(), tc.srcs...))
			assertJSON(t, "first pass", readJSON(t, firstPass), tc.want)
		})
	}
}

// A chain that sets no types would let tsgo include every package under the
// default typeRoots; the direct @types deps are written, an empty list if none.
func TestTsconfigStep_NoTypesWritesTheDirectTypesDeps(t *testing.T) {
	capture := readTestdata(t, "showconfig-no-roots.json")
	e := newExecroot(t, noRootsLeaf, capture)

	mustWriteTsconfig(t, e.tsconfigArgs(
		"-jsx=preserve", "-module=nodenext", "-types_dep=node", "-types_dep=react",
	))
	opts := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")["compilerOptions"]
	assertJSON(t, "types", opts.(map[string]any)["types"], `["node", "react"]`)
	assertJSON(t, "pkg.options.json", readJSON(t, binDir+"/pkg/pkg.options.json"),
		`{"target": "esnext", "jsx": "preserve", "module": "nodenext"}`)

	mustWriteTsconfig(t, e.tsconfigArgs("-jsx=preserve", "-module=nodenext"))
	opts = readJSON(t, binDir+"/pkg/pkg.tsconfig.json")["compilerOptions"]
	assertJSON(t, "types with no @types dep", opts.(map[string]any)["types"], `[]`)
}

func TestTsconfigStep_InheritedTypeRootsDoNotGainImplicitTypes(t *testing.T) {
	capture := readTestdata(t, "showconfig-no-types.json")
	e := newExecroot(t, noTypesLeaf, capture)

	mustWriteTsconfig(t, e.tsconfigArgs(
		"-jsx=preserve", "-module=nodenext", "-types_dep=node",
	))
	config := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
	opts := config["compilerOptions"].(map[string]any)
	if value, ok := opts["types"]; ok {
		t.Errorf("compilerOptions.types = %v, want unset: inherited typeRoots bounds inclusion", value)
	}
	assertJSON(t, "typeRoots", opts["typeRoots"], `["../base/typings"]`)
	assertJSON(t, "pkg.options.json", readJSON(t, binDir+"/pkg/pkg.options.json"),
		`{"target": "esnext", "jsx": "preserve", "jsxImportSource": "preact",
		  "module": "nodenext"}`)
}

func TestTsconfigStep_NoTsconfigRetainsResolvedBaselineOptions(t *testing.T) {
	e := newExecroot(t, noTypesLeaf, `{"compilerOptions": {"target": "es2022", "jsx": "react-jsx"}}`)
	args := []string{
		"-tsgo=" + e.tsgo,
		"-baseline=" + binDir + "/pkg/pkg.tsconfig_baseline.json",
		"-out=" + binDir + "/pkg/pkg.tsconfig.json",
		"-options=" + binDir + "/pkg/pkg.options.json",
		"-bin_dir=" + binDir,
		"-types_dep=node",
		"pkg/src/a.ts",
	}
	mustWriteTsconfig(t, args)

	config := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
	if _, ok := config["extends"]; ok {
		t.Fatal("resolved program must not reread its discovery config")
	}
	opts := config["compilerOptions"].(map[string]any)
	if _, ok := opts["paths"]; ok {
		t.Errorf("paths = %v, want none: no chain sets one", opts["paths"])
	}
	assertJSON(t, "types", opts["types"], `["node"]`)
	if _, ok := config["files"]; ok {
		t.Fatal("empty files without extends rejects an include-owned program with TS18002")
	}
	assertJSON(t, "include", config["include"], `["../../../../pkg/src/a.ts"]`)
	assertJSON(t, "pkg.options.json", readJSON(t, binDir+"/pkg/pkg.options.json"), `{"target": "es2022", "jsx": "react-jsx"}`)
}

// A JavaScript src sets allowJs; without it tsgo reads no .js the roots name.
func TestTsconfigStep_JavaScriptSrcSetsAllowJs(t *testing.T) {
	e := newExecroot(t, noTypesLeaf, readTestdata(t, "showconfig-no-types.json"))
	writeFile(t, "pkg/src/b.js", "export const b = 1;\n")

	mustWriteTsconfig(t, e.tsconfigArgs("-jsx=preserve", "-module=nodenext"))
	if got := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")["compilerOptions"].(map[string]any)["allowJs"]; got != nil {
		t.Errorf("allowJs = %v with no JavaScript src, want unset", got)
	}

	mustWriteTsconfig(t, append(
		e.tsconfigArgs("-jsx=preserve", "-module=nodenext"), "pkg/src/b.js"))
	if got := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")["compilerOptions"].(map[string]any)["allowJs"]; got != true {
		t.Errorf("allowJs = %v with a JavaScript src, want true", got)
	}
}

// The config turns the declaration emit off and carries no emit shape; the oxc
// mode's check keeps declaration on, which isolatedDeclarations needs (TS5069).
func TestTsconfigStep_WritesNoEmitShape(t *testing.T) {
	e := newExecroot(t, noTypesLeaf, readTestdata(t, "showconfig-no-types.json"))
	off := map[string]any{
		"declaration": false, "declarationMap": false,
		"emitDeclarationOnly": false, "declarationDir": nil,
	}
	unset := []string{"noEmit", "noEmitOnError", "outDir"}

	mustWriteTsconfig(t, e.tsconfigArgs("-jsx=preserve", "-module=nodenext"))
	config := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
	opts := config["compilerOptions"].(map[string]any)
	for key, want := range off {
		if got, ok := opts[key]; !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("compilerOptions.%s = %v (set: %v), want %v", key, got, ok, want)
		}
	}
	for _, key := range unset {
		if got, ok := opts[key]; ok {
			t.Errorf("compilerOptions.%s = %v, want unset", key, got)
		}
	}
	if got, want := opts["rootDir"], "../../../.."; got != want {
		t.Errorf("compilerOptions.rootDir = %v, want the exec root %q", got, want)
	}

	mustWriteTsconfig(t, e.tsconfigArgs(
		"-jsx=preserve", "-module=nodenext", "-isolated_declarations", "-lib_check",
	))
	config = readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
	opts = config["compilerOptions"].(map[string]any)
	for key, want := range map[string]any{
		"declaration":          true,
		"isolatedDeclarations": true,
		"skipLibCheck":         false,
		"emitDeclarationOnly":  false,
	} {
		if got := opts[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("compilerOptions.%s = %v, want %v", key, got, want)
		}
	}
}

// isolatedDeclarations requires declaration (TS5069).
func TestTsconfigStep_ChainIsolatedDeclarationsKeepsDeclaration(t *testing.T) {
	capture := readTestdata(t, "showconfig-isolated.json")
	e := newExecroot(t, isolatedLeaf, capture)

	mustWriteTsconfig(t, e.tsconfigArgs())
	config := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
	opts := config["compilerOptions"].(map[string]any)
	for key, want := range map[string]any{
		"declaration": true, "declarationMap": false,
		"emitDeclarationOnly": false, "declarationDir": nil,
	} {
		if got, ok := opts[key]; !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("compilerOptions.%s = %v (set: %v), want %v",
				key, got, ok, want)
		}
	}
	if got := opts["isolatedDeclarations"]; got != true {
		t.Errorf("compilerOptions.isolatedDeclarations = %v, want the inherited true", got)
	}
}

// A path-shaped types entry no input sits at fails the step, naming both places
// it looked, before tsgo's TS2688 could name no label.
func TestTsconfigStep_TypesEntryNowhereFails(t *testing.T) {
	e := newExecroot(t, chainLeaf, `{"compilerOptions": {"types": ["./missing.d.ts", "node"]}}`)

	err := writeTsconfig(e.tsconfigArgs())
	if err == nil {
		t.Fatal("writeTsconfig: want an error for a types entry nothing stages")
	}
	for _, want := range []string{`"./missing.d.ts"`, "pkg/missing.d.ts", binDir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if _, statErr := os.Stat(binDir + "/pkg/pkg.tsconfig.json"); statErr == nil {
		t.Error("a config was written although the types entry failed")
	}
}

func TestTsconfigStep_ShowConfigFailureIsTheError(t *testing.T) {
	e := newExecroot(t, chainLeaf, "")
	writeFile(t, e.tsgo, "#!/bin/sh\necho \"error TS5023: Unknown compiler option 'x'.\"\nexit 1\n")

	err := writeTsconfig(e.tsconfigArgs())
	if err == nil || !strings.Contains(err.Error(), "TS5023") || !strings.Contains(err.Error(), "--showConfig") {
		t.Errorf("writeTsconfig = %v, want tsgo's diagnostic and the command that printed it", err)
	}
}

// "." for the same directory, no leading "./"; "" and "." both name the exec
// root.
func TestRelativePath(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{binDir + "/pkg", "", "../../../.."},
		{binDir + "/pkg", ".", "../../../.."},
		{binDir + "/pkg", binDir, ".."},
		{binDir + "/pkg", binDir + "/pkg", "."},
		{binDir + "/pkg", binDir + "/pkg/node_modules", "node_modules"},
		{"pkg", "other/dir", "../other/dir"},
		{".", "pkg/src", "pkg/src"},
	}
	for _, tc := range cases {
		if got := relativePath(tc.from, tc.to); got != tc.want {
			t.Errorf("relativePath(%q, %q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
	for in, want := range map[string]string{"node_modules": "./node_modules", "../x": "../x", ".": ".", "/abs": "/abs", "./y": "./y"} {
		if got := explicitlyRelative(in); got != want {
			t.Errorf("explicitlyRelative(%q) = %q, want %q", in, got, want)
		}
	}
}

// The rule names a .tsx's emit before any action reads the tsconfig, from the
// ts_config's declaration; the step holds the two to one answer.
func TestTsconfigStep_JsxPreserveNeedsTheDeclaration(t *testing.T) {
	e := newExecroot(t, noTypesLeaf, readTestdata(t, "showconfig-no-types.json"))

	// Nothing in a .ts-only program is named by jsx: a plain file passes.
	mustWriteTsconfig(t, e.tsconfigArgs("-module=nodenext"))

	writeFile(t, "pkg/src/view.tsx", "export const v = 1;\n")
	err := writeTsconfig(append(
		e.tsconfigArgs("-module=nodenext"), "pkg/src/view.tsx"))
	if err == nil || !strings.Contains(err.Error(), `jsx = "preserve"`) ||
		!strings.Contains(err.Error(), "pkg/tsconfig.json") {
		t.Fatalf("writeTsconfig = %v, want the declaration named against "+
			"pkg/tsconfig.json", err)
	}
	if _, statErr := os.Stat(binDir + "/pkg/pkg.tsconfig.json"); statErr == nil {
		t.Error("a config was written although the declaration is missing")
	}

	mustWriteTsconfig(t, append(
		e.tsconfigArgs("-jsx=preserve", "-module=nodenext"), "pkg/src/view.tsx"))
	assertJSON(t, "pkg.options.json", readJSON(t, binDir+"/pkg/pkg.options.json"),
		`{"target": "esnext", "jsx": "preserve", "jsxImportSource": "preact",
		  "module": "nodenext"}`)
}

// The rule names a program's ES twins from the ts_config's module before any
// action reads the tsconfig; the step holds the two to one answer.
func TestTsconfigStep_ModuleTsgoEmitsNeedsTheDeclaration(t *testing.T) {
	e := newExecroot(t, noTypesLeaf, readTestdata(t, "showconfig-no-types.json"))

	err := writeTsconfig(e.tsconfigArgs())
	if err == nil || !strings.Contains(err.Error(), `module = "nodenext"`) ||
		!strings.Contains(err.Error(), "pkg/tsconfig.json") {
		t.Fatalf("writeTsconfig = %v, want the declaration named against "+
			"pkg/tsconfig.json", err)
	}
	if _, statErr := os.Stat(binDir + "/pkg/pkg.tsconfig.json"); statErr == nil {
		t.Error("a config was written although the declaration is missing")
	}

	// A declaration-only program has no emit for module to name.
	mustWriteTsconfig(t, []string{
		"-tsgo=" + e.tsgo, "-tsconfig=pkg/tsconfig.json",
		"-baseline=" + binDir + "/pkg/pkg.tsconfig_baseline.json",
		"-out=" + binDir + "/pkg/pkg.tsconfig.json",
		"-options=" + binDir + "/pkg/pkg.options.json",
		"-bin_dir=" + binDir, "pkg/globals.d.ts",
	})

	err = writeTsconfig(e.tsconfigArgs("-module=commonjs"))
	if err == nil ||
		!strings.Contains(err.Error(), `declares module = "commonjs"`) ||
		!strings.Contains(err.Error(), `declare module = "nodenext"`) {
		t.Errorf("writeTsconfig = %v, want the chain's module and the edit", err)
	}

	mustWriteTsconfig(t, e.tsconfigArgs("-module=nodenext"))
}

func TestTsconfigStep_ModuleDeclaredUnderAnESKindFails(t *testing.T) {
	e := newExecroot(t, chainLeaf, readTestdata(t, "showconfig-chain.json"))

	err := writeTsconfig(e.tsconfigArgs("-module=commonjs"))
	if err == nil || !strings.Contains(err.Error(), `"es6"`) ||
		!strings.Contains(err.Error(), "drop the attribute") {
		t.Errorf("writeTsconfig = %v, want the effective module and the edit", err)
	}
}

func TestTsconfigStep_JsxDeclaredPreserveUnderAnotherModeFails(t *testing.T) {
	e := newExecroot(t, chainLeaf, readTestdata(t, "showconfig-chain.json"))
	writeFile(t, "pkg/src/view.tsx", "export const v = 1;\n")

	err := writeTsconfig(
		append(e.tsconfigArgs("-jsx=preserve"), "pkg/src/view.tsx"))
	if err == nil || !strings.Contains(err.Error(), `"react-jsx"`) ||
		!strings.Contains(err.Error(), "drop the attribute") {
		t.Errorf("writeTsconfig = %v, want the effective jsx and the edit", err)
	}
}

func TestGeneratedAmbientIdentityDoesNotRequireDeclarationContents(t *testing.T) {
	e := newExecroot(t, chainLeaf, `{"compilerOptions":{"types":["./generated"]}}`)
	generated := binDir + "/pkg/generated.d.ts"
	if err := os.Remove(generated); err != nil {
		t.Fatal(err)
	}
	mustWriteTsconfig(t, e.tsconfigArgs("-type_input="+generated))
	config := readJSON(t, binDir+"/pkg/pkg.tsconfig.json")
	includes, _ := json.Marshal(config["include"])
	if !strings.Contains(string(includes), "generated.d.ts") {
		t.Fatalf("generated ambient missing: %s", includes)
	}
}

func TestUnrelatedIdentityCannotSupplyMissingAmbientInput(t *testing.T) {
	e := newExecroot(t, chainLeaf, `{"compilerOptions":{"types":["./missing"]}}`)
	err := writeTsconfig(e.tsconfigArgs("-type_input=" + binDir + "/other/missing.d.ts"))
	if err == nil || !strings.Contains(err.Error(), "no input") {
		t.Fatalf("missing input diagnostic: %v", err)
	}
}

func TestDeclaredDirectoryDoesNotInventAmbientChild(t *testing.T) {
	e := newExecroot(t, chainLeaf, `{"compilerOptions":{"types":["./tree"]}}`)
	tree := binDir + "/pkg/tree"
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeTsconfig(e.tsconfigArgs()); err == nil {
		t.Fatal("empty tree supplied ambient declaration")
	}
	writeFile(t, tree+"/index.d.ts", "declare const treeValue: string;\n")
	mustWriteTsconfig(t, e.tsconfigArgs())
}

func TestUnsupportedShowConfigCannotSilentlyDiscardCompilerOptions(t *testing.T) {
	for _, wire := range []string{`{"target":9,"module":200,"jsx":4}`, `{}`, `null`, `{"compilerOptions":null}`, `{"compilerOptions":[]}`, `{"compilerOptions":"es2022"}`} {
		t.Run(wire, func(t *testing.T) {
			if _, _, err := decodeShowConfig([]byte(wire)); err == nil || !strings.Contains(err.Error(), "unsupported --showConfig schema") {
				t.Fatalf("unsupported wire accepted: %s: %v", wire, err)
			}
		})
	}
	if _, _, err := decodeShowConfig([]byte(`{"compilerOptions":{"target":9}}`)); err == nil {
		t.Fatal("numeric enum accepted as a compiler option string")
	}
	if _, _, err := decodeShowConfig([]byte(`{"compilerOptions":{}}`)); err != nil {
		t.Fatalf("empty supported options rejected: %v", err)
	}
}
