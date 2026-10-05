package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// emitRoot lays an exec root out under a temp dir and makes it the working
// directory: two srcs under pkg/, one under bazel-out, and fake tools.
type emitRoot struct {
	oxc, oxcArgv, tsgo, tsgoArgv string
}

func newEmitRoot(t *testing.T, module string) *emitRoot {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"pkg/src/a.ts":                    "export const a: number = 1;\n",
		"pkg/src/view.tsx":                "export const v = <div />;\n",
		binDir + "/pkg/gen.ts":            "export const g = 2;\n",
		binDir + "/pkg/pkg.tsconfig.json": "{}",
		binDir + "/pkg/pkg.options.json": `{"target": "es2017", "jsx": "preserve",` +
			` "module": "` + module + `"}`,
		binDir + "/pkg/pkg/node_modules/x/i": "",
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(root, rel), body)
	}
	// The fake tsgo writes what tsc would into its --outDir: the maps' sources
	// are relative to the map's directory, seven below the exec root here.
	tsgoScript := `out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--outDir" ]; then out="$a"; fi; prev="$a"
done
readlink pkg/src/a.ts > "$0.root"
mkdir -p "$out/src"
for f in src/a.js src/a.d.ts src/view.jsx src/view.d.ts; do
  echo "$f" > "$out/$f"
done
up=../../../../../../..
cat > "$out/src/a.js.map" <<EOF
{"version":3,"file":"a.js","sourceRoot":"","names":[],"mappings":";;;AAAa",
"sources":["$up/pkg/src/a.ts"],"sourcesContent":["export const a = 1;\\n"]}
EOF
cat > "$out/src/view.jsx.map" <<EOF
{"version":3,"file":"view.jsx","sourceRoot":"","names":[],"mappings":";;;AAAa",
"sources":["$up/pkg/src/view.tsx"],"sourcesContent":["export const v = 1;\\n"]}
EOF
`
	e := &emitRoot{}
	e.oxc, e.oxcArgv = fakeTool(t, root, "oxc-bazel", "")
	e.tsgo, e.tsgoArgv = fakeTool(t, root, "tsgo", tsgoScript)
	t.Chdir(root)
	return e
}

func (e *emitRoot) args(extra ...string) []string {
	args := []string{
		"-options=" + binDir + "/pkg/pkg.options.json",
		"-tsconfig=" + binDir + "/pkg/pkg.tsconfig.json",
		"-source=pkg/src/a.ts",
		"-source=pkg/src/view.tsx",
		"-node_modules=" + binDir + "/pkg/pkg/node_modules",
		"-scratch=" + binDir + "/pkg/pkg.emit",
		"-out_dir=" + binDir + "/pkg",
		"-oxc=" + e.oxc,
		"-tsgo=" + e.tsgo,
	}
	return append(args, extra...)
}

// An ES module kind is oxc's: one run per root with the options' flags, and
// the root stripped so the emit lands at the src's package-relative path.
func TestEmitStep_ESModulesGoToOxcPerRoot(t *testing.T) {
	e := newEmitRoot(t, "esnext")
	err := runEmit(e.args("-root=pkg", "-root="+binDir+"/pkg", "-source_map",
		"-declarations", "pkg/src/a.ts", binDir+"/pkg/gen.ts", "pkg/src/view.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	// The last run recorded is the sorted last root, pkg.
	want := []string{
		"--files", "pkg/src/a.ts", "pkg/src/view.tsx", "--out-dir", binDir + "/pkg",
		"--strip-dir-prefix", "pkg", "--source-map",
		"--declaration", "--isolated-declarations",
		"--target", "es2017", "--jsx", "preserve", "--top-level-await",
	}
	if got := recordedArgs(t, e.oxcArgv); !reflect.DeepEqual(got, want) {
		t.Errorf("oxc ran with %q, want %q", got, want)
	}
	if _, err := os.Stat(e.tsgoArgv); err == nil {
		t.Error("tsgo ran for an ES module program")
	}
}

// commonjs is tsgo's: --noCheck from the program root of the -source files,
// the emit shape on the command line, each src's outputs moved from scratch.
func TestEmitStep_CommonJSGoesToTsgo(t *testing.T) {
	e := newEmitRoot(t, "commonjs")
	err := runEmit(e.args("-root=pkg", "-source_map", "-declarations", "-oxc=",
		"pkg/src/a.ts", "pkg/src/view.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	execroot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := recordedArgs(t, e.tsgo+".root")
	if want := filepath.Join(execroot, "pkg/src/a.ts"); got[0] != want {
		t.Errorf("pkg/src/a.ts in the program root -> %q, want %s", got, want)
	}
	want := []string{
		"--project", binDir + "/pkg/pkg.tsconfig.json", "--noCheck",
		"--noEmit", "false", "--emitDeclarationOnly", "false",
		"--declaration", "true", "--declarationMap", "false", "--sourceMap", "true",
		"--inlineSources", "true",
		"--outDir", binDir + "/pkg/pkg.emit/out", "--rootDir", "pkg",
	}
	if got := recordedArgs(t, e.tsgoArgv); !reflect.DeepEqual(got, want) {
		t.Errorf("tsgo ran with %q, want %q", got, want)
	}
	for _, out := range []string{
		"src/a.js", "src/a.js.map", "src/a.d.ts",
		"src/view.jsx", "src/view.jsx.map", "src/view.d.ts",
	} {
		if _, err := os.Stat(filepath.Join(binDir, "pkg", out)); err != nil {
			t.Errorf("%s was not moved into the output directory: %v", out, err)
		}
	}
	for m, want := range map[string]string{
		"src/a.js.map": "pkg/src/a.ts", "src/view.jsx.map": "pkg/src/view.tsx",
	} {
		got := readSourceMap(t, filepath.Join(binDir, "pkg", m)).Sources
		if !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("%s names sources %q, want %q", m, got, want)
		}
	}
	_, err = os.Stat(filepath.Join(binDir, "pkg", "pkg.emit"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the scratch directory survived the step: %v", err)
	}
	if _, err := os.Stat(e.oxcArgv); err == nil {
		t.Error("oxc ran for a commonjs program")
	}
}

// -es_modules is oxc's transform whatever the module, with oxc's inputs alone:
// the vitest runner's program, and the ES twins of one tsgo emits.
func TestEmitStep_ESModulesFlagIsOxcWhateverTheModule(t *testing.T) {
	e := newEmitRoot(t, "commonjs")
	err := runEmit([]string{
		"-options=" + binDir + "/pkg/pkg.options.json",
		"-out_dir=" + binDir + "/pkg/pkg.es", "-oxc=" + e.oxc, "-root=pkg",
		"-es_modules", "pkg/src/a.ts", "pkg/src/view.tsx",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--files", "pkg/src/a.ts", "pkg/src/view.tsx",
		"--out-dir", binDir + "/pkg/pkg.es", "--strip-dir-prefix", "pkg",
		"--target", "es2017", "--jsx", "preserve",
	}
	if got := recordedArgs(t, e.oxcArgv); !reflect.DeepEqual(got, want) {
		t.Errorf("oxc ran with %q, want %q", got, want)
	}
	if _, err := os.Stat(e.tsgoArgv); err == nil {
		t.Error("tsgo ran under -es_modules")
	}
}

func TestEmitStep_MixedRootsPreserveRuntimeAndDeclarationGeometry(t *testing.T) {
	type emissionCase struct {
		declarationsOnly bool
		sourceRoot       string
	}
	for _, root := range []string{"pkg", ""} {
		cases := []emissionCase{{}, {declarationsOnly: true}}
		if root == "" {
			for _, policy := range []string{"relative", "absolute", "url"} {
				cases = append(cases, emissionCase{declarationsOnly: true, sourceRoot: policy})
			}
		}
		for _, scenario := range cases {
			declarationsOnly := scenario.declarationsOnly
			name := fmt.Sprintf("root=%s/declarations=%v", root, declarationsOnly)
			if scenario.sourceRoot != "" {
				name += "/sourceRoot=" + scenario.sourceRoot
			}
			t.Run(name, func(t *testing.T) {
				e := newEmitRoot(t, "commonjs")
				directory, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				authored := strings.TrimPrefix("pkg/src/a", root+"/")
				generated := strings.TrimPrefix("pkg/gen", root+"/")
				script := `out=""; previous=""
for argument in "$@"; do
 if [ "$previous" = "--outDir" ]; then out="$argument"; fi
 previous="$argument"
done
mkdir -p "$out/` + path.Dir(authored) + `" "$out/foreign"
for suffix in js d.ts; do
 echo authored > "$out/` + authored + `.$suffix"
 echo foreign > "$out/foreign/value.$suffix"
 echo generated > "$out/` + generated + `.$suffix"
done
`
				for _, source := range []string{"pkg/src/a.ts", "foreign/value.ts", binDir + "/pkg/gen.ts"} {
					logical := strings.TrimPrefix(source, binDir+"/")
					stem := strings.TrimPrefix(strings.TrimSuffix(logical, ".ts"), root+"/")
					for _, suffix := range []string{".js.map", ".d.ts.map"} {
						from := filepath.Join(binDir, "pkg/pkg.emit/out", stem+suffix)
						relative, err := filepath.Rel(filepath.Dir(from), logical)
						if err != nil {
							t.Fatal(err)
						}
						mapped := sourceMap{Version: 3, Sources: []string{filepath.ToSlash(relative)}}
						if scenario.sourceRoot != "" {
							mapped.Sources = []string{filepath.Base(source)}
							switch scenario.sourceRoot {
							case "relative":
								base, err := filepath.Rel(filepath.Dir(from), filepath.Dir(logical))
								if err != nil {
									t.Fatal(err)
								}
								mapped.SourceRoot = filepath.ToSlash(base) + "/"
							case "absolute":
								mapped.SourceRoot = filepath.ToSlash(filepath.Join(directory, filepath.Dir(logical))) + "/"
							case "url":
								mapped.SourceRoot = (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(directory, filepath.Dir(logical))) + "/"}).String()
							}
						}
						data, err := json.Marshal(mapped)
						if err != nil {
							t.Fatal(err)
						}
						script += "cat > \"$out/" + stem + suffix + "\" <<'MAP'\n" + string(data) + "\nMAP\n"
					}
				}
				e.tsgo, e.tsgoArgv = fakeTool(t, directory, "mixed-tsgo", script)
				args := []string{"-root=" + root, "-root=" + binDir}
				if root != "" {
					args[1] += "/" + root
				}
				if declarationsOnly {
					args = append(args, "-declarations_only", "-declaration_map", "-oxc=")
				} else {
					args = append(args, "-source_map")
				}
				args = append(args, "pkg/src/a.ts", binDir+"/pkg/gen.ts")
				outputs := map[string]string{"src/a": "authored", "gen": "generated"}
				origins := map[string]string{"src/a": "pkg/src/a.ts", "gen": binDir + "/pkg/gen.ts"}
				if root == "" {
					writeFile(t, "foreign/value.ts", "export const value = 42;")
					args = append([]string{"-source=foreign/value.ts"}, args...)
					args = append(args, "foreign/value.ts")
					outputs = map[string]string{"pkg/src/a": "authored", "pkg/gen": "generated", "foreign/value": "foreign"}
					origins = map[string]string{"pkg/src/a": "pkg/src/a.ts", "pkg/gen": binDir + "/pkg/gen.ts", "foreign/value": "foreign/value.ts"}
				}
				if err := runEmit(e.args(args...)); err != nil {
					t.Fatal(err)
				}
				suffix := ".js"
				if declarationsOnly {
					suffix = ".d.ts"
				}
				if _, err := os.Stat(filepath.Join(binDir, "pkg", "pkg.emit")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("scratch directory survived emission: %v", err)
				}
				for output, want := range outputs {
					contents, err := os.ReadFile(filepath.Join(binDir, "pkg", output+suffix))
					if err != nil || strings.TrimSpace(string(contents)) != want {
						t.Fatalf("%s lost its source identity: %q, %v", output, contents, err)
					}
					mapPath := filepath.Join(binDir, "pkg", output+suffix+".map")
					mapped := readSourceMap(t, mapPath)
					if declarationsOnly {
						if len(mapped.Sources) != 1 {
							t.Fatalf("%s map has %q, want one original source", output, mapped.Sources)
						}
						mapURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(directory, mapPath))}
						baseURL, err := url.Parse(mapped.SourceRoot)
						if err != nil {
							t.Fatal(err)
						}
						sourceURL, err := url.Parse(mapped.Sources[0])
						if err != nil {
							t.Fatal(err)
						}
						resolved := mapURL.ResolveReference(baseURL).ResolveReference(sourceURL)
						if resolved.Scheme != "file" || resolved.Host != "" {
							t.Fatalf("unexpected source URL: %s", resolved)
						}
						actual, err := os.Stat(filepath.FromSlash(resolved.Path))
						if err != nil {
							t.Fatalf("%s declaration map cannot reach original source %q: %v", output, origins[output], err)
						}
						expected, err := os.Stat(origins[output])
						if err != nil || !os.SameFile(actual, expected) {
							t.Fatalf("%s declaration map resolves to %q, want exact original File %q: %v", output, resolved, origins[output], err)
						}
					} else if !reflect.DeepEqual(mapped.Sources, []string{origins[output]}) {
						t.Fatalf("%s map points to %q, want original source %q", output, mapped.Sources, origins[output])
					}
				}
				arguments := recordedArgs(t, e.tsgoArgv)
				if slices.Contains(arguments, "--noCheck") == declarationsOnly || slices.Contains(arguments, "--noEmitOnError") != declarationsOnly {
					t.Fatalf("declaration checking changed: %q", arguments)
				}
			})
		}
	}
}

func readSourceMap(t *testing.T, name string) sourceMap {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var m sourceMap
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, data)
	}
	return m
}

func TestEmitMap_GeneratedProjectionPreservesExplicitRemoteSourceRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	const from = binDir + "/pkg/scratch/generated.d.ts.map"
	const to = binDir + "/pkg/generated.d.ts.map"
	writeFile(t, from, `{"version":3,"sourceRoot":"https://sources.example/pkg/","sources":["generated.ts"],"mappings":"AAAA"}`)
	if err := moveMap(from, to, binDir+"/pkg/generated.ts", true); err != nil {
		t.Fatal(err)
	}
	mapped := readSourceMap(t, to)
	if mapped.SourceRoot != "https://sources.example/pkg/" || !reflect.DeepEqual(mapped.Sources, []string{"generated.ts"}) {
		t.Fatalf("explicit source location was replaced by a compiler path: %+v", mapped)
	}
}

func TestEmitStep_ExitCodeIsTheTools(t *testing.T) {
	e := newEmitRoot(t, "esnext")
	writeFile(t, e.oxc, "#!/bin/sh\nexit 3\n")
	if err := os.Chmod(e.oxc, 0o755); err != nil {
		t.Fatal(err)
	}
	err := runEmit(e.args("-root=pkg", "pkg/src/a.ts"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("runEmit = %v, want oxc's exit status 3", err)
	}
}

func TestEffectiveESModuleEmitDoesNotRequireTypeProgram(t *testing.T) {
	e := newEmitRoot(t, "esnext")
	var args []string
	for _, arg := range e.args() {
		if strings.HasPrefix(arg, "-tsconfig=") || strings.HasPrefix(arg, "-scratch=") || strings.HasPrefix(arg, "-tsgo=") || strings.HasPrefix(arg, "-source=") || strings.HasPrefix(arg, "-node_modules=") {
			continue
		}
		args = append(args, arg)
	}
	if err := runEmit(append(args, "-root=pkg", "pkg/src/a.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.oxcArgv); err != nil {
		t.Fatal(err)
	}
	writeFile(t, binDir+"/pkg/pkg.options.json", `{"module":"commonjs"}`)
	if err := runEmit(append(args, "-root=pkg", "pkg/src/a.ts")); err == nil || !strings.Contains(err.Error(), "-tsconfig") {
		t.Fatalf("CommonJS missing program diagnostic: %v", err)
	}
}

func TestIncompatibleRuntimeScopeStopsBothEmittersBeforePublication(t *testing.T) {
	for _, module := range []string{"esnext", "commonjs"} {
		t.Run(module, func(t *testing.T) {
			e := newEmitRoot(t, module)
			writeFile(t, "pkg/package.json", `{"imports":{"#value":"./value.ts"}}`)
			writeFile(t, binDir+"/pkg/package.json", `{"imports":{"#value":"./value.ts"}}`)
			check, err := json.Marshal(runtimeScopeCheck{Source: "pkg/package.json", Runtime: binDir + "/pkg/package.json", Targets: []runtimeTarget{{"./value.ts", "./value.js"}}})
			if err != nil {
				t.Fatal(err)
			}
			err = runEmit(e.args("-root=pkg", "-runtime_scope="+string(check), "pkg/src/a.ts"))
			if err == nil || !strings.Contains(err.Error(), "needs \"./value.js\"") {
				t.Fatalf("error = %v", err)
			}
			for _, argv := range []string{e.oxcArgv, e.tsgoArgv} {
				if _, err := os.Stat(argv); !os.IsNotExist(err) {
					t.Fatalf("compiler invoked before scope validation: %s: %v", argv, err)
				}
			}
		})
	}
}
