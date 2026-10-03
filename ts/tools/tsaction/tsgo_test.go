package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

const (
	programRoot  = binDir + "/pkg/app.program"
	rootImporter = binDir + "/node_modules"
	subImporter  = binDir + "/pkg/sub/node_modules"
	manifest     = binDir + "/pkg/app.ownership"
)

func tsgoArgs(rest ...string) []string {
	flags := []string{
		"-root=" + programRoot,
		"-source=pkg/a.ts",
		"-source=pkg/package.json",
		"-node_modules=" + subImporter,
		"-node_modules=" + rootImporter,
		"-check=" + manifest,
	}
	return append(flags, rest...)
}

// A fake exec root: the action's sources beside files it does not name, the
// importers' node_modules and pkg's outputs (both manifests, a declaration).
func newTsgoExecroot(t *testing.T, script string) (root, argv string) {
	t.Helper()
	root = linkedDir(t)
	for rel, body := range map[string]string{
		"pkg/a.ts":                        "export {};\n",
		"pkg/lib.ts":                      "export {};\n",
		"pkg/package.json":                `{"exports": "./lib.ts"}` + "\n",
		"pkg/sub/b.ts":                    "export {};\n",
		"pkg/other/c.ts":                  "export {};\n",
		rootImporter + "/zod/index.d.ts":  "export {};\n",
		subImporter + "/ms/index.d.ts":    "export {};\n",
		binDir + "/pkg/app.tsconfig.json": "{}\n",
		binDir + "/pkg/package.json":      `{"exports": "./lib.ts"}` + "\n",
		binDir + "/pkg/app.package.json":  `{"exports": "./lib.js"}` + "\n",
		binDir + "/pkg/lib.package.json":  `{"exports": "./lib.js"}` + "\n",
		binDir + "/pkg/lib.d.ts":          "export {};\n",
	} {
		writeFile(t, filepath.Join(root, rel), body)
	}
	writeFile(t, filepath.Join(root, manifest),
		"label\t//pkg:app\nown\tpkg/a.ts\n")
	if err := os.MkdirAll(filepath.Join(root, "external/tsgo"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, argv = fakeTool(t, filepath.Join(root, "external/tsgo"), "tsc", script)
	t.Chdir(root)
	return root, argv
}

func linkedDir(t *testing.T) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "execroot")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	return link
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// The root holds each -source at its path, the output tree whole, the chain's
// node_modules, and no file the action did not name; then it is gone, stamped.
func TestTsgoStep_RunsFromAProgramRootOfTheSourcesNamed(t *testing.T) {
	root, argv := newTsgoExecroot(t,
		"pwd >> \"$0.argv\"\nreadlink node_modules >> \"$0.argv\"\n"+
			"readlink pkg/sub/node_modules >> \"$0.argv\"\n"+
			"ls | tr '\\n' ' ' >> \"$0.argv\"\necho >> \"$0.argv\"\n"+
			"test -d pkg -a ! -L pkg && echo pkg-is-real >> \"$0.argv\"\n"+
			"readlink pkg/a.ts >> \"$0.argv\"\n"+
			"readlink bazel-out >> \"$0.argv\"\n"+
			"test -e pkg/lib.ts || echo lib-absent >> \"$0.argv\"\n"+
			"test -e pkg/other || echo other-absent >> \"$0.argv\"\n")
	stamp := binDir + "/pkg/app.tscheck"

	err := runTsgo(tsgoArgs("-stamp="+stamp, "--", "external/tsgo/tsc",
		"--project", binDir+"/pkg/app.tsconfig.json", "--noEmit"))
	if err != nil {
		t.Fatalf("runTsgo: %v", err)
	}

	got := recordedArgs(t, argv)
	if want := []string{"--project", binDir + "/pkg/app.tsconfig.json", "--noEmit"}; !reflect.DeepEqual(got[:3], want) {
		t.Errorf("tsgo ran with %q, want %q", got[:3], want)
	}
	if want := filepath.Join(root, programRoot); got[3] != want {
		t.Errorf("tsgo ran in %s, want the program root %s", got[3], want)
	}
	if want := filepath.Join(root, rootImporter); got[4] != want {
		t.Errorf("node_modules -> %s, want the root importer's %s", got[4], want)
	}
	if want := filepath.Join(root, subImporter); got[5] != want {
		t.Errorf("pkg/sub/node_modules -> %s, want the importer's %s", got[5], want)
	}
	if want := "bazel-out node_modules pkg "; got[6] != want {
		t.Errorf("the program root lists %q, want %q: the sources' "+
			"directories, the output tree and the root importer alone", got[6], want)
	}
	if got[7] != "pkg-is-real" {
		t.Errorf("pkg is not a real directory on the way to pkg/sub: %q", got[7:])
	}
	if want := filepath.Join(root, "pkg/a.ts"); got[8] != want {
		t.Errorf("pkg/a.ts -> %s, want the source at its exec path %s", got[8], want)
	}
	if want := filepath.Join(root, "bazel-out"); got[9] != want {
		t.Errorf("bazel-out -> %s, want the output tree whole %s", got[9], want)
	}
	want := []string{"lib-absent", "other-absent"}
	if !reflect.DeepEqual(got[10:12], want) {
		t.Errorf("the root holds files the action does not name: %q, want %q",
			got[10:12], want)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("no stamp after a passing run: %v", err)
	}
	if _, err := os.Stat(programRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the program root outlived the action: stat = %v", err)
	}
}

func TestTsgoStep_ASourceWithoutALogicalCoordinateCannotWriteThroughTheOutputTree(t *testing.T) {
	_, argv := newTsgoExecroot(t, "echo ran\n")

	err := runTsgo(tsgoArgs("-source=bazel-out/unplaced.ts", "--",
		"external/tsgo/tsc"))
	if err == nil || !strings.Contains(err.Error(), "bazel-out/unplaced.ts") {
		t.Errorf("runTsgo = %v, want the output-tree source refused", err)
	}
	if _, err := os.Stat(argv); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tsgo ran after the refusal: stat = %v", err)
	}
}

func TestProgramRoot_GeneratedSourcesKeepDeclaredIdentityAndRejectDuplicateCoordinates(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var sources []string
	for _, file := range []string{"pkg/input.ts", "pkg/helper.mjs", "pkg/data.json", "pkg/package.json"} {
		generated := binDir + "/" + file
		writeFile(t, generated, "declared generated input")
		writeFile(t, file, "undeclared checkout twin")
		sources = append(sources, generated)
		writeFile(t, binDir+"/dependency/"+filepath.Base(file), "dependency publication")
	}
	dependency := overlayArg(t, binDir+"/dependency", "pkg")
	if err := layOutProgramRoot(programRoot, sources, nil, nil, []string{dependency[len("-overlay="):]}, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range sources {
		logical := strings.TrimPrefix(file, binDir+"/")
		if actual := throughRoot(filepath.Join(root, programRoot), root, root, logical); actual != file {
			t.Fatalf("%s resolves to %s, want declared File %s", logical, actual, file)
		}
		body, err := os.ReadFile(filepath.Join(programRoot, logical))
		if err != nil || string(body) != "declared generated input" {
			t.Fatalf("%s reads checkout bytes: %q, %v", logical, body, err)
		}
	}
	err := layOutProgramRoot(programRoot, append(sources, "pkg/input.ts"), nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "declare one source") {
		t.Fatalf("two declared Files claimed the same coordinate: %v", err)
	}
}

func TestProgramRoot_EachInheritedImporterResolvesItsOwnLinksBeforeTheChain(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	nearest, chainRoot := binDir+"/pkg/app/node_modules", binDir+"/node_modules"
	first, second := binDir+"/libs/a/node_modules", binDir+"/libs/b/node_modules"
	for importer, names := range map[string][]string{
		nearest:   {"conflict", "chain-only"},
		chainRoot: {"zod"},
		first:     {"conflict", "first-only"},
		second:    {"second-only"},
	} {
		for _, name := range names {
			writeFile(t, importer+"/"+name+"/index.d.ts", importer)
		}
	}
	if err := layOutProgramRoot(programRoot, nil, []string{nearest, chainRoot}, []string{first, second}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for at, want := range map[string]string{
		"libs/a/node_modules/conflict":    first,
		"libs/a/node_modules/first-only":  first,
		"libs/a/node_modules/chain-only":  nearest,
		"libs/b/node_modules/second-only": second,
		"libs/b/node_modules/conflict":    nearest,
		"libs/b/node_modules/chain-only":  nearest,
		"pkg/app/node_modules/conflict":   nearest,
		"node_modules/zod":                chainRoot,
	} {
		body, err := os.ReadFile(filepath.Join(programRoot, at, "index.d.ts"))
		if err != nil || string(body) != want {
			t.Errorf("%s resolves to %q (%v), want %s", at, body, err, want)
		}
	}
	for _, at := range []string{"libs/a/node_modules/zod", "libs/b/node_modules/zod"} {
		if _, err := os.Lstat(filepath.Join(programRoot, at)); err == nil {
			t.Errorf("%s copies the root importer the ancestor walk already reaches", at)
		}
	}
	if info, err := os.Lstat(filepath.Join(programRoot, "pkg/app/node_modules")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the chain's nearest importer is no longer a plain link: %v", err)
	}
}

func TestTsgoStep_ExcludedGeneratedRootCannotEscapeValidation(t *testing.T) {
	_, _ = newTsgoExecroot(t, "echo pkg/a.ts\n")
	generated := binDir + "/pkg/generated.ts"
	writeFile(t, generated, "const unchecked: never = 0;\n")
	writeFile(t, "pkg/tsconfig.json", `{"exclude":["generated.ts"]}`)
	writeFile(t, manifest, "label\t//pkg:app\nown\t"+generated+"\n")
	err := runTsgo(tsgoArgs("-source="+generated, "-tsconfig=pkg/tsconfig.json", "--", "external/tsgo/tsc"))
	if err == nil || !strings.Contains(err.Error(), "excluded by") || !strings.Contains(err.Error(), generated) {
		t.Fatalf("generated source was left unchecked: %v", err)
	}
}

func TestTsgoStep_GeneratedImporterCannotBypassStrictDeps(t *testing.T) {
	root, _ := newTsgoExecroot(t, "cat <<'LISTING'\npkg/helper.ts\n   Imported via \"./helper\" from file 'pkg/generated.ts'\npkg/generated.ts\nLISTING\n")
	generated := binDir + "/pkg/generated.ts"
	writeFile(t, generated, "import './helper';\n")
	writeFile(t, "pkg/helper.ts", "export {};\n")
	writeFile(t, manifest, "label\t//pkg:app\nown\t"+generated+"\nfile\t//pkg:helper\tpkg/helper.ts\n")
	err := runTsgo(tsgoArgs("-source="+generated, "-source=pkg/helper.ts", "--", "external/tsgo/tsc"))
	var undeclared *undeclaredDeps
	if !errors.As(err, &undeclared) || !strings.Contains(err.Error(), generated) || !strings.Contains(err.Error(), "//pkg:helper") {
		t.Fatalf("logical generated importer skipped ownership checking: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, generated)); err != nil {
		t.Fatal(err)
	}
}

func TestTsgoStep_GeneratedLintArgumentsUseTheCompilerNamespace(t *testing.T) {
	_, argv := newTsgoExecroot(t, "readlink \"$1\" >> \"$0.argv\"\n")
	generated := binDir + "/pkg/generated.mjs"
	writeFile(t, generated, "export const value = 42;\n")
	args := tsgoArgs("-source="+generated, "--", "external/tsgo/tsc", generated)
	args = slices.DeleteFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "-check=") })
	if err := runTsgo(args); err != nil {
		t.Fatal(err)
	}
	got := recordedArgs(t, argv)
	if len(got) != 2 || got[0] != "pkg/generated.mjs" || !strings.HasSuffix(got[1], "/"+generated) {
		t.Fatalf("lint input does not use the compiler namespace and exact File: %q", got)
	}
}

func TestProgramRoot_OverlayOriginDoesNotDependOnOutputFilename(t *testing.T) {
	for _, file := range []string{"value.d.ts", "value.d.mts", "value.d.cts", "value.json", "package.json"} {
		for _, tc := range []struct {
			name         string
			second       string
			directory    bool
			reverse      bool
			protected    bool
			wantConflict bool
		}{
			{name: "original source coordinate"},
			{name: "distinct file cannot replace occupied origin", second: "distinct", wantConflict: true},
			{name: "reversed files cannot replace occupied origin", second: "distinct", reverse: true, wantConflict: true},
			{name: "expanded tree cannot replace occupied origin", second: "distinct", directory: true, wantConflict: true},
			{name: "reversed tree cannot replace occupied origin", second: "distinct", directory: true, reverse: true, wantConflict: true},
			{name: "canonical alias coalesces", second: "alias"},
			{name: "expanded canonical alias coalesces", second: "alias", directory: true},
			{name: "explicit root keeps precedence", second: "distinct", protected: true},
			{name: "explicit root keeps precedence over tree", second: "distinct", directory: true, protected: true},
		} {
			t.Run(file+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				t.Chdir(root)
				output := binDir + "/app/private-output-" + file
				logical := "foreign/" + file
				body, otherBody := "export declare const value: 42;\n", "export declare const value: 42;\n"
				if strings.HasSuffix(file, ".json") {
					body, otherBody = `{"value":42}`, `{"other":"second"}`
				}
				writeFile(t, output, body)
				overlays := []string{overlayArg(t, output, logical)[len("-overlay="):]}
				next := "bazel-out/other-fastbuild/bin/foreign/" + file
				if tc.second != "" {
					if tc.second == "alias" {
						if err := os.MkdirAll(filepath.Dir(next), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(filepath.Join(root, output), next); err != nil {
							t.Fatal(err)
						}
					} else {
						writeFile(t, next, otherBody)
					}
					from, to := next, logical
					if tc.directory {
						from, to = filepath.Dir(next), filepath.Dir(logical)
					}
					overlays = append(overlays, overlayArg(t, from, to)[len("-overlay="):])
				}
				var sources []string
				selected := output
				if tc.reverse {
					slices.Reverse(overlays)
					selected = next
				}
				if tc.protected {
					writeFile(t, logical, body)
					sources = []string{logical}
					selected = logical
				}
				err := layOutProgramRoot(programRoot, sources, nil, nil, overlays, nil)
				if tc.wantConflict {
					if err == nil {
						t.Fatal("distinct compiler inputs silently selected the last overlay")
					}
					for _, want := range []string{logical, filepath.FromSlash(output), filepath.FromSlash(next), "publish the shared module once"} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("compiler input conflict = %v, want %q", err, want)
						}
					}
				} else if err != nil {
					t.Fatal(err)
				}
				actual, err := os.Readlink(filepath.Join(programRoot, logical))
				if err != nil || actual != filepath.Join(root, selected) {
					t.Fatalf("overlay origin selected %q, %v; want exact output %q", actual, err, selected)
				}
				if resolved := throughRoot(filepath.Join(root, programRoot), root, root, logical); resolved != selected {
					t.Fatalf("overlay ownership = %q, want exact artifact %q", resolved, selected)
				}
			})
		}
	}
	t.Run("dangling occupied declaration is not an empty destination", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		output, next, logical := binDir+"/app/value.d.ts", binDir+"/other/value.d.ts", "foreign/value.d.ts"
		writeFile(t, output, "export {};\n")
		writeFile(t, next, "export {};\n")
		if err := layOutProgramRoot(programRoot, nil, nil, nil, []string{overlayArg(t, output, logical)[len("-overlay="):]}, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(output); err != nil {
			t.Fatal(err)
		}
		from := filepath.Join(root, next)
		st, err := os.Stat(from)
		if err != nil {
			t.Fatal(err)
		}
		err = overlayPath(programRoot, filepath.Join(root, programRoot), from, filepath.FromSlash(logical), st, nil, false)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "resolve existing") {
			t.Fatalf("dangling declaration was overwritten: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(programRoot, logical)); err != nil || target != filepath.Join(root, output) {
			t.Fatalf("failed admission changed the declaration link: %q, %v", target, err)
		}
	})

}

func TestTsgoStep_OriginalScopeSurvivesRuntimeAndPublicationOverlays(t *testing.T) {
	for _, selectedFiles := range []bool{false, true} {
		for _, explicitManifest := range []bool{false, true} {
			name := "directory/original compiler scope"
			if selectedFiles {
				name = "selected files/original compiler scope"
			}
			if explicitManifest {
				name = strings.Replace(name, "original compiler scope", "explicit as-built manifest", 1)
			}
			t.Run(name, func(t *testing.T) {
				root, argv := newTsgoExecroot(t,
					"readlink pkg/package.json >> \"$0.argv\"\n"+
						"readlink pkg/lib.d.ts >> \"$0.argv\"\n"+
						"readlink pkg/child/value.d.ts >> \"$0.argv\"\n"+
						"readlink pkg/child/data.json >> \"$0.argv\"\n"+
						"for f in package.json runtime-only.json value.js; do test -e pkg/child/$f && echo $f >> \"$0.argv\"; done\n"+
						"test -e pkg/lib.ts || echo lib-source-absent >> \"$0.argv\"\n"+
						"readlink pkg/sub/node_modules >> \"$0.argv\"\n"+
						"test -e pkg/app.program && echo root-linked >> \"$0.argv\" || "+
						"echo root-skipped >> \"$0.argv\"\n"+
						"test -d pkg/sub -a ! -L pkg/sub && echo sub-is-real >> \"$0.argv\"\n")

				writeFile(t, filepath.Join(root, binDir, "pkg/package.json"), `{"exports":"./lib.js"}`)
				for file, contents := range map[string]string{
					"value.d.ts":        "export declare const value: number;\n",
					"data.json":         `{"value":42}`,
					"package.json":      `{"type":"module"}`,
					"runtime-only.json": `{"private":true}`,
					"value.js":          "export const value = 42;\n",
				} {
					writeFile(t, filepath.Join(root, binDir, "pkg/child", file), contents)
				}
				args := []string{overlayArg(t, binDir+"/pkg", "pkg")}
				if selectedFiles {
					args = nil
					for _, file := range []string{"package.json", "lib.d.ts", "child/value.d.ts", "child/data.json", "child/value.js"} {
						args = append(args, overlayArg(t, binDir+"/pkg/"+file, "pkg/"+file))
					}
				}
				wantScope := filepath.Join(root, "pkg/package.json")
				if explicitManifest {
					args = append(args, "-manifest="+binDir+"/pkg/app.package.json")
				}
				args = append(args, "--", "external/tsgo/tsc")
				err := runTsgo(tsgoArgs(args...))
				if err != nil {
					t.Fatalf("runTsgo: %v", err)
				}
				got := recordedArgs(t, argv)[1:]
				want := []string{
					wantScope,
					filepath.Join(root, binDir, "pkg/lib.d.ts"),
					filepath.Join(root, binDir, "pkg/child/value.d.ts"),
					filepath.Join(root, binDir, "pkg/child/data.json"),
				}
				if !selectedFiles {
					want = append(want, "package.json", "runtime-only.json")
				}
				want = append(want, "lib-source-absent", filepath.Join(root, subImporter), "root-skipped", "sub-is-real")
				if !reflect.DeepEqual(got, want) {
					t.Errorf("the program root reads\n%q\nwant\n%q", got, want)
				}
			})
		}
	}
}

// The overlay lays a dep's declarations, manifest and data over its package
// and none of its JavaScript: a program reads a dep through its declarations.
func TestTsgoStep_LaysNoJavaScriptOverThePackage(t *testing.T) {
	laid := []string{"gen/m.d.ts", "gen/n.d.mts", "gen/data.json", "package.json"}
	left := []string{"gen/m.js", "gen/n.mjs", "gen/util.cjs", "gen/view.jsx"}
	files := strings.Join(laid, " ") + " " + strings.Join(left, " ")
	root, argv := newTsgoExecroot(t,
		"for f in "+files+"; do "+
			"if test -e pkg/$f; then echo \"$f\" >> \"$0.argv\"; fi; done\n")
	for _, rel := range strings.Fields(files) {
		writeFile(t, filepath.Join(root, binDir, "pkg", rel), "\n")
	}

	err := runTsgo(tsgoArgs(overlayArg(t, binDir+"/pkg", "pkg"), "--", "external/tsgo/tsc"))
	if err != nil {
		t.Fatalf("runTsgo: %v", err)
	}
	if got := recordedArgs(t, argv)[1:]; !reflect.DeepEqual(got, laid) {
		t.Errorf("the overlay lays %q over pkg, want %q: the declarations, "+
			"the manifest and the data, and no JavaScript", got, laid)
	}
}

// An overlaid declaration is listed by its path under the root; the check reads
// it through the link. Without the overlay the listed file is nobody's.
func TestTsgoStep_ChecksAnOverlaidFileByItsOutput(t *testing.T) {
	listing := "pkg/lib.d.ts\n" +
		"   Imported via \"./lib\" from file 'pkg/a.ts'\n" +
		"pkg/a.ts\n   Root file specified for compilation\n"
	root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\n")
	writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"), listing)
	writeFile(t, filepath.Join(root, manifest),
		"label\t//pkg:app\nown\tpkg/a.ts\ndirect\t//pkg:lib\n"+
			"file\t//pkg:lib\t"+binDir+"/pkg/lib.d.ts\n")

	err := runTsgo(tsgoArgs(overlayArg(t, binDir+"/pkg", "pkg"), "--", "external/tsgo/tsc"))
	if err != nil {
		t.Errorf("runTsgo with the dep overlaid: %v", err)
	}
	err = runTsgo(tsgoArgs("--", "external/tsgo/tsc"))
	if err == nil || !strings.Contains(err.Error(), "no src, dep or npm package") {
		t.Errorf("runTsgo without the overlay = %v, want the file unowned", err)
	}
}

// The manifest -manifest lays at pkg/package.json is listed by that path; the
// check reads it through the link, so the dep's <name>.package.json owns it.
func TestTsgoStep_ChecksALaidOverManifestByItsOutput(t *testing.T) {
	listing := "pkg/package.json\n" +
		"   Imported via \"./package.json\" from file 'pkg/a.ts'\n" +
		"pkg/a.ts\n   Root file specified for compilation\n"
	root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\n")
	writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"), listing)
	asWritten := "label\t//pkg:app\nown\tpkg/a.ts\ndirect\t//pkg:lib\n" +
		"file\t//pkg:lib\t" + binDir + "/pkg/package.json\n"
	args := tsgoArgs(overlayArg(t, binDir+"/pkg/package.json", "pkg/package.json"),
		"-manifest="+binDir+"/pkg/lib.package.json", "--", "external/tsgo/tsc")
	args = slices.DeleteFunc(args, func(arg string) bool { return arg == "-source=pkg/package.json" })

	writeFile(t, filepath.Join(root, manifest),
		asWritten+"file\t//pkg:lib\t"+binDir+"/pkg/lib.package.json\n")
	if err := runTsgo(args); err != nil {
		t.Errorf("runTsgo with the manifest as built owned: %v", err)
	}
	writeFile(t, filepath.Join(root, manifest), asWritten)
	err := runTsgo(args)
	if err == nil || !strings.Contains(err.Error(), "no src, dep or npm package") {
		t.Errorf("runTsgo with the src alone owned = %v, want the file unowned", err)
	}
}

func TestTsgoStep_ChecksAFileUnderALinkByItsStoreTree(t *testing.T) {
	for _, layout := range []struct{ name, pkg, sandbox, spelling string }{
		{"real", "shared", "", "link"},
		{"memberSandbox", "shared", "member", "link"},
		{"physicalMemberSandbox", "shared", "member", "physical"},
		{"scopedPhysicalMemberSandbox", "@types/shared", "member", "physical"},
		{"scopedRelativePhysicalMemberSandbox", "@types/shared", "member", "physicalRelative"},
		{"scopedTreeSandbox", "@scope/shared", "tree", "link"},
		{"scopedPhysicalTreeSandbox", "@types/shared", "tree", "physical"},
		{"scopedOuterImporter", "@types/shared", "importer", "link"},
		{"relativeStore", "shared", "", "relative"},
		{"absoluteStore", "shared", "", "absolute"},
	} {
		t.Run(layout.name, func(t *testing.T) {
			root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\n")
			key := strings.ReplaceAll(layout.pkg, "/", "+") + "@0.0.0"
			storeA := rootImporter + "/.pnpm/" + key + "/node_modules/" + layout.pkg
			storeB := subImporter + "/.pnpm/" + key + "/node_modules/" + layout.pkg
			for importer, tree := range map[string]string{rootImporter: storeA, subImporter: storeB} {
				at := filepath.Join(root, tree, "lib/index.d.ts")
				if layout.sandbox == "" || layout.sandbox == "importer" {
					writeFile(t, at, "export {};\n")
				} else {
					real := filepath.Join(t.TempDir(), tree, "lib/index.d.ts")
					writeFile(t, real, "export {};\n")
					if layout.sandbox == "tree" {
						at = filepath.Join(root, tree)
						real = filepath.Dir(filepath.Dir(real))
					}
					if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(real, at); err != nil {
						t.Fatal(err)
					}
				}
				link := filepath.Join(root, importer, layout.pkg)
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				target, err := filepath.Rel(filepath.Dir(link), filepath.Join(root, tree))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				if layout.sandbox == "importer" {
					at := filepath.Join(root, importer)
					real := filepath.Join(t.TempDir(), importer)
					if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(at, real); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(real, at); err != nil {
						t.Fatal(err)
					}
				}
			}
			own := "label\t//pkg:app\nown\tpkg/a.ts\nown\tpkg/sub/b.ts\n"
			ownership := own + "npm-direct\t" + layout.pkg + "\t" + storeA + "\n" +
				"npm\t" + layout.pkg + "\t@npm//:shared\t" + storeB + "\n"
			writeFile(t, filepath.Join(root, manifest), ownership)
			for _, c := range []struct{ name, from, tree, link string }{
				{"allowedA", "pkg/a.ts", storeA, "node_modules/" + layout.pkg},
				{"rejectedB", "pkg/sub/b.ts", storeB, "pkg/sub/node_modules/" + layout.pkg},
			} {
				t.Run(c.name, func(t *testing.T) {
					to := c.link + "/lib/index.d.ts"
					switch layout.spelling {
					case "relative":
						var err error
						to, err = filepath.Rel(programRoot, c.tree+"/lib/index.d.ts")
						if err != nil {
							t.Fatal(err)
						}
					case "absolute":
						to = filepath.Join(root, c.tree, "lib/index.d.ts")
					case "physical", "physicalRelative":
						to = realpath(t, filepath.Join(root, c.tree, "lib/index.d.ts"))
						if layout.spelling == "physicalRelative" {
							var err error
							to, err = filepath.Rel(filepath.Join(root, programRoot), to)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					listing := filepath.ToSlash(to) + "\n   Imported via \"" + layout.pkg + "\" from file '" + c.from + "'\n" +
						c.from + "\n   Root file specified for compilation\n"
					writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"), listing)
					err := runTsgo(tsgoArgs("-source=pkg/sub/b.ts", "--", "external/tsgo/tsc"))
					if c.name == "allowedA" {
						if err != nil {
							t.Fatalf("selected store A was rejected: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "add @npm//:shared to deps") {
						t.Fatalf("undeclared same-key store B = %v, want its owner", err)
					}
				})
			}
			writeFile(t, filepath.Join(root, manifest), ownership+"npm-direct\t"+layout.pkg+"\t"+storeB+"\n")
			if err := runTsgo(tsgoArgs("-source=pkg/sub/b.ts", "--", "external/tsgo/tsc")); err != nil {
				t.Fatalf("declared store B was rejected: %v", err)
			}
			writeFile(t, filepath.Join(root, manifest), own+"npm-direct\t"+layout.pkg+"\t"+storeA+"\n")
			err := runTsgo(tsgoArgs("-source=pkg/sub/b.ts", "--", "external/tsgo/tsc"))
			if err == nil || !strings.Contains(err.Error(), "npm closure does not hold") {
				t.Fatalf("foreign same-key store B = %v, want a closure error", err)
			}
			writeFile(t, filepath.Join(root, manifest), own)
			err = runTsgo(tsgoArgs("-source=pkg/sub/b.ts", "--", "external/tsgo/tsc"))
			if err == nil || !strings.Contains(err.Error(), "npm closure does not hold") {
				t.Fatalf("unowned store B = %v, want a closure error", err)
			}
		})
	}
}

func TestTsgoStep_RejectsAmbiguousPhysicalStoreWithoutLosingFileIdentity(t *testing.T) {
	for _, layout := range []string{"tree", "member"} {
		t.Run(layout, func(t *testing.T) {
			root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\n")
			const pkg = "@types/shared"
			storeA := rootImporter + "/.pnpm/@types+shared@0.0.0/node_modules/" + pkg
			storeB := subImporter + "/.pnpm/@types+shared@0.0.0/node_modules/" + pkg
			physicalStore := filepath.Join(t.TempDir(), ".pnpm")
			physical := filepath.Join(physicalStore, "@types+shared@0.0.0/node_modules", pkg)
			writeFile(t, filepath.Join(physical, "index.d.ts"), "export {};\n")
			for importer, tree := range map[string]string{rootImporter: storeA, subImporter: storeB} {
				at, target := filepath.Join(root, importer, ".pnpm"), physicalStore
				if layout == "member" {
					at, target = filepath.Join(root, tree, "index.d.ts"), filepath.Join(physical, "index.d.ts")
				}
				if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, at); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, filepath.Join(root, manifest), "label\t//pkg:app\nown\tpkg/a.ts\n"+
				"npm-direct\t"+pkg+"\t"+storeA+"\n"+
				"npm\t"+pkg+"\t@npm//:shared\t"+storeB+"\n")
			for _, c := range []struct{ name, tree, want string }{
				{"directFile", storeA, ""},
				{"indirectFile", storeB, "add @npm//:shared to deps"},
				{"ambiguousPhysical", physical, "multiple declared npm store inputs"},
			} {
				t.Run(c.name, func(t *testing.T) {
					to := filepath.Join(c.tree, "index.d.ts")
					if !filepath.IsAbs(to) {
						var err error
						to, err = filepath.Rel(programRoot, to)
						if err != nil {
							t.Fatal(err)
						}
					}
					listing := filepath.ToSlash(to) + "\n   Imported via \"" + pkg + "\" from file 'pkg/a.ts'\n" +
						"pkg/a.ts\n   Root file specified for compilation\n"
					writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"), listing)
					err := runTsgo(tsgoArgs("--", "external/tsgo/tsc"))
					if c.want == "" {
						if err != nil {
							t.Fatalf("known File identity was rejected: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), c.want) {
						t.Fatalf("runTsgo = %v, want %q", err, c.want)
					}
				})
			}
		})
	}
}

func TestBinRelative(t *testing.T) {
	for _, c := range []struct{ binPath, want, importer string }{
		{binDir + "/pkg/sub/node_modules", "pkg/sub/node_modules", "pkg/sub"},
		{binDir + "/node_modules", "node_modules", ""},
		{binDir + "/pkg", "pkg", ""},
		{binDir, "", ""},
	} {
		if got := binRelative(c.binPath); got != c.want {
			t.Errorf("binRelative(%q) = %q, want %q", c.binPath, got, c.want)
		}
		if strings.HasSuffix(c.binPath, "node_modules") {
			if got := importerDir(c.binPath); got != c.importer {
				t.Errorf("importerDir(%q) = %q, want %q", c.binPath, got, c.importer)
			}
		}
	}
}

// The tool's exit status is the step's, no stamp is written for it, and the
// program root is removed all the same.
func TestTsgoStep_ExitCodeIsTheToolsAndNoStamp(t *testing.T) {
	newTsgoExecroot(t, "exit 3\n")
	stamp := binDir + "/pkg/app.tscheck"

	err := runTsgo(tsgoArgs("-stamp="+stamp, "--", "external/tsgo/tsc"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("runTsgo = %v, want tsgo's exit status 3", err)
	}
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stamp was written for a failing run: stat = %v", err)
	}
	if _, err := os.Stat(programRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the program root outlived the action: stat = %v", err)
	}
}

// With -check, the listing tsgo prints is read against the manifest and never
// reaches the action's stdout; an undeclared edge is the step's failure.
func TestTsgoStep_ChecksTheListingAgainstTheManifest(t *testing.T) {
	listing := "bazel-out/k8-fastbuild/bin/pkg/hidden.d.ts\n" +
		"   Imported via \"./hidden\" from file 'pkg/a.ts'\n" +
		"pkg/a.ts\n   Root file specified for compilation\n"
	root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\n")
	writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"), listing)
	stamp := binDir + "/pkg/app.tscheck"
	args := tsgoArgs("-stamp="+stamp, "--", "external/tsgo/tsc")

	writeFile(t, filepath.Join(root, manifest),
		"label\t//pkg:app\nown\tpkg/a.ts\ndirect\t//pkg:lib\n"+
			"file\t//pkg:hidden\t"+binDir+"/pkg/hidden.d.ts\n")
	err := runTsgo(args)
	if err == nil || !strings.Contains(err.Error(), "add //pkg:hidden to deps") {
		t.Errorf("runTsgo = %v, want the undeclared edge naming //pkg:hidden", err)
	}
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stamp was written for a failing check: stat = %v", err)
	}

	writeFile(t, filepath.Join(root, manifest),
		"label\t//pkg:app\nown\tpkg/a.ts\ndirect\t//pkg:hidden\n"+
			"file\t//pkg:hidden\t"+binDir+"/pkg/hidden.d.ts\n")
	if err := runTsgo(args); err != nil {
		t.Errorf("runTsgo with the dep declared: %v", err)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("no stamp after a passing check: %v", err)
	}
}

// tsgo's own failure is relayed with its diagnostics and without the listing.
func TestTsgoStep_AFailingTsgoRelaysItsDiagnostics(t *testing.T) {
	root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\nexit 2\n")
	writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"),
		"pkg/a.ts\n   Root file specified for compilation\n"+
			"pkg/a.ts(1,8): error TS2307: Cannot find module './gone'.\n")
	stdout := captureStdout(t)

	err := runTsgo(tsgoArgs("--", "external/tsgo/tsc"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("runTsgo = %v, want tsgo's exit status 2", err)
	}
	out := stdout()
	if !strings.Contains(out, "error TS2307") {
		t.Errorf("the diagnostic is not relayed:\n%s", out)
	}
	if strings.Contains(out, "Root file specified") {
		t.Errorf("the listing reached stdout:\n%s", out)
	}
}

// A failing tsgo that printed no diagnostic -- a usage message, a crash line
// -- has its whole output relayed; only a parsed listing stays off stdout.
func TestTsgoStep_AFailingTsgoWithNoDiagnosticRelaysItsOutput(t *testing.T) {
	newTsgoExecroot(t, "echo 'tsc: unknown option --explainFile'\nexit 1\n")
	stdout := captureStdout(t)

	err := runTsgo(tsgoArgs("--", "external/tsgo/tsc"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("runTsgo = %v, want tsgo's exit status 1", err)
	}
	if out := stdout(); !strings.Contains(out, "unknown option") {
		t.Errorf("tsgo's output is not relayed:\n%s", out)
	}
}

// Without -check the tool runs from the same root with its output relayed and
// nothing parsed: the declare's run, which lists no edges and writes no stamp.
func TestTsgoStep_WithoutCheckRunsTheToolAlone(t *testing.T) {
	_, argv := newTsgoExecroot(t, "pwd >> \"$0.argv\"\necho declared\n")
	stdout := captureStdout(t)

	err := runTsgo([]string{"-root=" + programRoot, "-source=pkg/a.ts",
		"-node_modules=" + rootImporter, "--", "external/tsgo/tsc",
		"--project", binDir + "/pkg/app.tsconfig.json", "--emitDeclarationOnly"})
	if err != nil {
		t.Fatalf("runTsgo without -check: %v", err)
	}
	got := recordedArgs(t, argv)
	want := []string{
		"--project", binDir + "/pkg/app.tsconfig.json", "--emitDeclarationOnly",
	}
	if !reflect.DeepEqual(got[:3], want) {
		t.Errorf("tsgo ran with %q, want %q", got[:3], want)
	}
	if !strings.HasSuffix(got[3], programRoot) {
		t.Errorf("tsgo ran in %s, want the program root", got[3])
	}
	if out := stdout(); !strings.Contains(out, "declared") {
		t.Errorf("the tool's output is not relayed:\n%s", out)
	}
	if _, err := os.Stat(programRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the program root outlived the action: stat = %v", err)
	}
}

// A src the chain's exclude names and no import brings in is outside the
// program: the step fails naming the src and the entry, and writes no stamp.
func TestTsgoStep_ASrcTheChainExcludesAndTheProgramLacksFails(t *testing.T) {
	root, _ := newTsgoExecroot(t, "cat "+binDir+"/pkg/listing.txt\n")
	writeFile(t, filepath.Join(root, "pkg/b.ts"), "export const b = 1;\n")
	writeFile(t, filepath.Join(root, "pkg/tsconfig.json"),
		`{"include": ["*.ts"], "exclude": ["b.ts"]}`+"\n")
	writeFile(t, filepath.Join(root, manifest),
		"label\t//pkg:app\nown\tpkg/a.ts\nown\tpkg/b.ts\n")
	rootA := "pkg/a.ts\n   Matched by include pattern '../../../../pkg/*.ts'" +
		" in '" + binDir + "/pkg/app.tsconfig.json'\n"
	writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"), rootA)
	stamp := binDir + "/pkg/app.tscheck"
	args := tsgoArgs("-tsconfig=pkg/tsconfig.json", "-source=pkg/b.ts",
		"-stamp="+stamp, "--", "external/tsgo/tsc")

	err := runTsgo(args)
	for _, want := range []string{
		"//pkg:app: srcs the program never read", "pkg/tsconfig.json",
		"  pkg/b.ts\texcluded by \"b.ts\"",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("runTsgo = %v, want the message with %q", err, want)
		}
	}
	if _, err := os.Stat(stamp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stamp was written for a src the program lacks: %v", err)
	}

	writeFile(t, filepath.Join(root, binDir, "pkg/listing.txt"),
		"pkg/b.ts\n   Imported via \"./b\" from file 'pkg/a.ts'\n"+rootA)
	if err := runTsgo(args); err != nil {
		t.Errorf("runTsgo with b.ts imported into the program: %v", err)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("no stamp after a passing check: %v", err)
	}
}

func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	return func() string {
		os.Stdout = saved
		w.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

func TestProgramCopiesConfigImportsWithoutLosingWorkspacePaths(t *testing.T) {
	root, _ := newTsgoExecroot(t, "")
	for file, body := range map[string]string{
		"lint/config.mjs":          "import './plugins/rule.mjs';",
		"lint/plugins/rule.mjs":    "import './options.mjs';",
		"lint/plugins/options.mjs": "export default {};",
	} {
		writeFile(t, filepath.Join(root, file), body)
	}
	tool, _ := fakeTool(t, root, "lint-tool", `set -eu
[ "$1" = "--config" ]
[ "$2" = "lint/config.mjs" ]
[ "$3" = "pkg/a.ts" ]
[ -f "$3" ]
[ ! -L lint/config.mjs ]
[ ! -L lint/plugins/rule.mjs ]
[ ! -L lint/plugins/options.mjs ]
[ -f node_modules/zod/index.d.ts ]
[ -f pkg/sub/node_modules/ms/index.d.ts ]
[ -f bazel-out/k8-fastbuild/bin/pkg/lib.d.ts ]
[ -f bazel-out/k8-fastbuild/bin/pkg/app.tsconfig.json ]
[ -f pkg/tsconfig.json ]
[ ! -L pkg/tsconfig.json ]
`)
	stamp := binDir + "/pkg/app.tslint"
	args := []string{
		"-root=" + programRoot,
		"-source=pkg/a.ts",
		"-source=lint/config.mjs",
		"-copy=lint/config.mjs",
		"-copy=lint/plugins/rule.mjs",
		"-copy=lint/plugins/options.mjs",
		"-discover-tsconfig=" + binDir + "/pkg/app.tsconfig.json",
		"-node_modules=" + subImporter,
		"-node_modules=" + rootImporter,
		"-stamp=" + stamp,
		"--", tool, "--config", "lint/config.mjs", "pkg/a.ts",
	}
	if err := runTsgo(args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(programRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("program root was not cleaned up: %v", err)
	}
	if got, err := os.ReadFile("lint/config.mjs"); err != nil || string(got) != "import './plugins/rule.mjs';" {
		t.Fatalf("source config changed: %q, %v", got, err)
	}
}

func TestLintDiscoveryDoesNotReenterOriginalOrNestedConfigs(t *testing.T) {
	for _, original := range []string{"pkg/tsconfig.json", "configs/browser.json"} {
		t.Run(original, func(t *testing.T) {
			root, _ := newTsgoExecroot(t, "")
			base := `{"compilerOptions":{"types":["node"]}}`
			project := `{"extends":"../base.json","include":["../pkg/**/*.ts"]}`
			generated := binDir + "/pkg/app.tsconfig.json"
			generatedBody := `{"compilerOptions":{"module":"ESNext","types":["node"],"rootDirs":["../../../..",".."]},"include":["../../../../pkg/**/*.ts"]}`
			writeFile(t, filepath.Join(root, "base.json"), base)
			writeFile(t, filepath.Join(root, original), project)
			writeFile(t, filepath.Join(root, generated), generatedBody)
			nested := `{"compilerOptions":{"module":"CommonJS"}}`
			writeFile(t, filepath.Join(root, "pkg/nested/tsconfig.json"), nested)
			sources := []string{original, "base.json", "pkg/a.ts", "pkg/nested/tsconfig.json"}
			if err := layOutProgramRoot(programRoot, sources, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := discoverProgramConfig(programRoot, generated, sources); err != nil {
				t.Fatal(err)
			}
			shim := filepath.Join(programRoot, "pkg/tsconfig.json")
			data, err := os.ReadFile(shim)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 2 || fields["extends"] != fileRelative("pkg", generated) {
				t.Fatalf("discovery must reference the canonical program: %s", data)
			}
			if !reflect.DeepEqual(fields["compilerOptions"], map[string]any{"noEmit": true}) {
				t.Fatalf("lint must validate JavaScript without attempting to overwrite its inputs: %s", data)
			}
			resolved, err := tsconfig.Resolve(shim)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Module != "ESNext" || resolved.Types == nil || !reflect.DeepEqual(*resolved.Types, []string{"node"}) || resolved.Include == nil || !reflect.DeepEqual(*resolved.Include, []string{"../../../../pkg/**/*.ts"}) {
				t.Fatalf("lost generated or original compiler configuration: %+v", resolved)
			}
			nestedShim := filepath.Join(programRoot, "pkg/nested/tsconfig.json")
			nestedResolved, err := tsconfig.Resolve(nestedShim)
			if err != nil {
				t.Fatal(err)
			}
			if len(nestedResolved.Extends) != 1 {
				t.Fatalf("nested discovery must reference one canonical program: %+v", nestedResolved)
			}
			nestedTarget, ok := tsconfig.ResolveExtends(filepath.Dir(nestedShim), nestedResolved.Extends[0])
			if !ok {
				t.Fatalf("nested discovery cannot resolve %q", nestedResolved.Extends[0])
			}
			nestedInfo, err := os.Stat(nestedTarget)
			if err != nil {
				t.Fatal(err)
			}
			generatedInfo, err := os.Stat(generated)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(nestedInfo, generatedInfo) {
				t.Fatalf("nested discovery references %q instead of %q", nestedTarget, generated)
			}
			want := *resolved
			want.Extends = nestedResolved.Extends
			if !reflect.DeepEqual(nestedResolved, &want) {
				t.Fatalf("nearer declared config bypassed the target program: %+v", nestedResolved)
			}
			for file, want := range map[string]string{"base.json": base, original: project, generated: generatedBody, "pkg/nested/tsconfig.json": nested} {
				got, err := os.ReadFile(file)
				if err != nil || string(got) != want {
					t.Fatalf("changed original input %s: %q, %v", file, got, err)
				}
			}
		})
	}
}

func TestProgramToolEnvironmentKeepsAbsoluteExecutableAndRunfiles(t *testing.T) {
	root, _ := newTsgoExecroot(t, "")
	sidecar := "tools/side car=declared"
	writeFile(t, filepath.Join(root, sidecar), "#!/bin/sh\ncat \"$0.runfiles/payload\"\n")
	if err := os.Chmod(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, sidecar+".runfiles/payload"), "declared runfile")
	tool, _ := fakeTool(t, root, "lint-tool", `set -eu
case "$LINT_AUXILIARY" in /*) ;; *) exit 20;; esac
[ "$LINT_AUXILIARY" = "$1" ]
[ "$("$LINT_AUXILIARY")" = "declared runfile" ]
[ -f pkg/a.ts ]
`)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINT_AUXILIARY", "must be replaced")
	if err := runTsgo([]string{
		"-root=" + programRoot,
		"-source=pkg/a.ts",
		"-tool-env=LINT_AUXILIARY=" + sidecar,
		"--", tool, filepath.Join(cwd, sidecar),
	}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LINT_AUXILIARY"); got != "must be replaced" {
		t.Fatalf("tool environment escaped the child process: %q", got)
	}
}

func TestFormatterCannotRepairBadInputAndReportSuccess(t *testing.T) {
	root, _ := newTsgoExecroot(t, "printf 'fixed\\n' > pkg/a.ts\n")
	tool := filepath.Join(root, "external/tsgo/tsc")
	before, err := os.ReadFile("pkg/a.ts")
	if err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(binDir, "format.stamp")
	err = runTsgo([]string{"-root=" + programRoot, "-source=pkg/a.ts", "-copy=pkg/a.ts", "-verify-copies", "-stamp=" + stamp, "--", tool})
	if err == nil || !strings.Contains(err.Error(), "validation changed input pkg/a.ts") {
		t.Fatalf("got %v", err)
	}
	after, err := os.ReadFile("pkg/a.ts")
	if err != nil || string(after) != string(before) {
		t.Fatalf("source changed: %q, %v", after, err)
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatalf("failed formatter wrote stamp: %v", err)
	}
}

func TestFormatterPathsStayAbsoluteInsideProgramRoot(t *testing.T) {
	root, _ := newTsgoExecroot(t, "case \"$1\" in /*) ;; *) exit 19;; esac\nprintf 'fixed\\n' > \"$1\"\n")
	tool := filepath.Join(root, "external/tsgo/tsc")
	err := runTsgo([]string{"-root=" + programRoot, "-source=pkg/a.ts", "-copy=pkg/a.ts", "-absolute-copy-args", "-verify-copies", "--", tool, "pkg/a.ts"})
	if err == nil || !strings.Contains(err.Error(), "validation changed input pkg/a.ts") {
		t.Fatalf("formatter did not receive the absolute copied input: %v", err)
	}
}

func TestProgramWithoutEmitRejectsScopeThatRewritesItsNativeTarget(t *testing.T) {
	_, argv := newTsgoExecroot(t, "echo ran\n")
	writeFile(t, binDir+"/pkg/package.json", `{"exports":"./lib.js"}`)
	check, err := json.Marshal(runtimeScopeCheck{Source: "pkg/package.json", Runtime: binDir + "/pkg/package.json", Targets: []runtimeTarget{{"./lib.ts", "./lib.ts"}}})
	if err != nil {
		t.Fatal(err)
	}
	err = runTsgo(tsgoArgs("-runtime_scope="+string(check), "--", "external/tsgo/tsc"))
	if err == nil || !strings.Contains(err.Error(), "needs \"./lib.ts\"") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(argv); !os.IsNotExist(err) {
		t.Fatalf("checker invoked before scope validation: %v", err)
	}
}

func overlayArg(t *testing.T, physical, logical string) string {
	t.Helper()
	data, err := json.Marshal([]string{physical, logical})
	if err != nil {
		t.Fatal(err)
	}
	return "-overlay=" + string(data)
}
