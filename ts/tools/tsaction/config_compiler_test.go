package main

import (
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestCompilerRetainsInheritedOptionsAndIncludePriority(t *testing.T) {
	for name, test := range map[string]struct {
		ambient, roots     string
		hasTypes, hasRoots bool
	}{
		"absent ambient lists": {"", "", false, false},
		"empty ambient lists":  {`,"types":[]`, `,"typeRoots":[]`, true, true},
	} {
		t.Run(name, func(t *testing.T) { compilerConfigFixture(t, test.ambient, test.roots, test.hasTypes, test.hasRoots) })
	}
}

func TestCompilerGeneratedRootLosesDeclaredPackageImportScope(t *testing.T) {
	compiler, err := runfiles.Rlocation(os.Getenv("TSGO_RLOCATION"))
	if err != nil {
		t.Fatal(err)
	}
	compiler, err = filepath.Abs(compiler)
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"preserve", "nodenext"} {
		t.Run(module, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			packageType, helperFile, helperBody := "module", "pkg/helper.mjs", "export const helper = 42;\n"
			if module == "nodenext" {
				packageType, helperFile, helperBody = "commonjs", "pkg/helper.cjs", "exports.helper = 42;\n"
			}
			const generated = binDir + "/pkg/config.ts"
			const config = binDir + "/pkg/config.tsconfig.json"
			const options = binDir + "/pkg/config.options.json"
			const ownership = binDir + "/pkg/config.ownership"
			const output = binDir + "/pkg/declarations"
			for name, body := range map[string]string{
				"baseline.json":    `{"compilerOptions":{"target":"es2022","module":"` + module + `","strict":true,"skipLibCheck":true,"types":[]}}`,
				"pkg/package.json": `{"type":"` + packageType + `","imports":{"#helper":"./` + filepath.Base(helperFile) + `"}}`,
				helperFile:         helperBody,
				generated:          "import { helper } from '#helper'; export const value: number = helper;\n",
				ownership:          "label\t//pkg:config\nown\t" + generated + "\nown\t" + helperFile + "\nown\tpkg/package.json\n",
			} {
				writeFile(t, name, body)
			}
			projection := overlayArg(t, generated, "pkg/config.ts")
			for _, poisoned := range []bool{false, true} {
				if poisoned {
					writeFile(t, "pkg/config.ts", "import '#undeclared-stale-helper'; export const value: never = 0;\n")
				}
				if err := writeTsconfig([]string{
					"-tsgo=" + compiler, "-baseline=baseline.json", "-out=" + config,
					"-options=" + options, "-bin_dir=" + binDir, "-source_only",
					generated, helperFile,
				}); err != nil {
					t.Fatal(err)
				}
				if err := runTsgo([]string{
					"-root=" + programRoot, "-source=pkg/package.json", "-source=" + helperFile,
					projection, "-check=" + ownership,
					"--", compiler, "--project", config, "--noEmit", "--explainFiles", "--pretty", "false",
				}); err != nil {
					t.Fatalf("generated root lost declared package import (poisoned checkout %t): %v", poisoned, err)
				}
				if err := runEmit([]string{
					"-options=" + options, "-tsgo=" + compiler, "-tsconfig=" + config,
					"-source=pkg/package.json", "-source=" + helperFile, projection,
					"-root=" + binDir + "/pkg", "-out_dir=" + output,
					"-scratch=" + binDir + "/pkg/declare", "-declarations_only", generated,
				}); err != nil {
					t.Fatalf("declaration emit lost generated root scope: %v", err)
				}
				declaration, err := os.ReadFile(output + "/config.d.ts")
				if err != nil || !strings.Contains(string(declaration), "export declare const value: number;") {
					t.Fatalf("declaration was not projected to its declared output: %q, %v", declaration, err)
				}
				if module == "nodenext" {
					if err := runEmit([]string{
						"-options=" + options, "-tsgo=" + compiler, "-tsconfig=" + config,
						"-source=pkg/package.json", "-source=" + helperFile, projection,
						"-root=" + binDir + "/pkg", "-out_dir=" + output,
						"-scratch=" + binDir + "/pkg/commonjs", generated,
					}); err != nil {
						t.Fatalf("CommonJS emit lost generated root scope: %v", err)
					}
					body, err := os.ReadFile(output + "/config.js")
					if err != nil || !strings.Contains(string(body), `require("#helper")`) {
						t.Fatalf("CommonJS output lost its runtime import: %q, %v", body, err)
					}
				}
				if err := layOutProgramRoot(programRoot, []string{"pkg/package.json", helperFile}, nil, nil, []string{projection[len("-overlay="):]}, nil); err != nil {
					t.Fatal(err)
				}
				for logical, original := range map[string]string{"pkg/config.ts": generated, helperFile: helperFile, "pkg/package.json": "pkg/package.json"} {
					if actual := throughRoot(filepath.Join(root, programRoot), root, root, logical); actual != original {
						t.Fatalf("%s changed File identity: got %s, want %s", logical, actual, original)
					}
				}
				if err := os.RemoveAll(programRoot); err != nil {
					t.Fatal(err)
				}
				if poisoned {
					body, err := os.ReadFile("pkg/config.ts")
					if err != nil || !strings.Contains(string(body), "#undeclared-stale-helper") {
						t.Fatalf("compiler view changed the checkout twin: %q, %v", body, err)
					}
				}
			}
		})
	}
}

func compilerConfigFixture(t *testing.T, ambient, roots string, hasTypes, hasRoots bool) {
	compiler, err := runfiles.Rlocation(os.Getenv("TSGO_RLOCATION"))
	if err != nil {
		t.Fatal(err)
	}
	compiler, err = filepath.Abs(compiler)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(compiler, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("compiler identity: %v: %s", err, version)
	}
	t.Logf("selected compiler: %s", strings.TrimSpace(string(version)))
	executable, err := os.ReadFile(compiler)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("selected compiler sha256: %x", sha256.Sum256(executable))
	root := t.TempDir()
	t.Chdir(root)
	for path, body := range map[string]string{
		"baseline.json": `{"compilerOptions":{"target":"es2022","module":"preserve","jsx":"react-jsx","strict":true,"skipLibCheck":true}}`,
		"node_modules/config-package/package.json": `{"name":"config-package","tsconfig":"config.json"}`,
		"node_modules/config-package/config.json":  `{"compilerOptions":{"target":"es2017","jsx":"preserve"` + roots + `}}`,
		"pkg/tsconfig.json":                        `{"extends":["../baseline.json","config-package"],"compilerOptions":{"allowJs":true` + ambient + `},"files":["src/explicit.ts"],"include":["src/**/*","src/value.mjs"],"exclude":["src/excluded.ts","src/explicit.ts"]}`,
		"pkg/src/main.ts":                          `export const values = new Set<string>();`,
		"pkg/src/view.tsx":                         `declare namespace JSX { interface IntrinsicElements { span: {} } } const element = <span />;`,
		"pkg/src/value.mjs":                        `export const value = 1;`,
		"pkg/src/value.d.mts":                      `export declare const value: number;`,
		"pkg/src/explicit.ts":                      `export const explicit = 1;`,
		"pkg/src/excluded.ts":                      `const excluded: string = 1;`,
	} {
		writeFile(t, path, body)
	}
	wire, err := exec.Command(compiler, "--showConfig", "-p", "pkg/tsconfig.json").CombinedOutput()
	if err != nil {
		t.Fatalf("compiler metadata: %v: %s", err, wire)
	}
	t.Logf("compiler metadata wire: %s", wire)
	out := binDir + "/pkg/action.tsconfig.json"
	options := binDir + "/pkg/options.json"
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-tsgo=" + compiler, "-baseline=baseline.json", "-tsconfig=pkg/tsconfig.json",
		"-out=" + out, "-options=" + options, "-bin_dir=" + binDir, "-jsx=preserve",
		"pkg/src/main.ts", "pkg/src/view.tsx", "pkg/src/value.mjs", "pkg/src/value.d.mts", "pkg/src/explicit.ts", "pkg/src/excluded.ts",
	}
	if strings.TrimSpace(string(version)) == "Version 7.0.0-dev.20251229.1" {
		if err := writeTsconfig(args); err == nil || !strings.Contains(err.Error(), "unsupported --showConfig schema") {
			t.Fatalf("legacy compiler must fail explicitly: %v", err)
		}
		for _, file := range []string{out, options} {
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported compiler left output %s: %v", file, err)
			}
		}
		t.Log("unsupported compiler rejected; no config or options output")
		return
	}
	metadata, _, err := decodeShowConfig(wire)
	if err != nil {
		t.Fatal(err)
	}
	if (metadata.Types != nil) != hasTypes || (metadata.TypeRoots != nil) != hasRoots {
		t.Fatalf("ambient presence changed: %+v", metadata)
	}
	t.Logf("compiler-owned ambient metadata types=%v typeRoots=%v", metadata.Types, metadata.TypeRoots)
	if err := writeTsconfig(args); err != nil {
		t.Fatal(err)
	}
	run := func(project string, flags ...string) string {
		t.Helper()
		cmd := exec.Command(compiler, append([]string{"--project", project, "--pretty", "false"}, flags...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("compiler %s %v: %v\n%s", project, flags, err, output)
		}
		return string(output)
	}
	listedRoots := func(project string) []string {
		t.Helper()
		var files []string
		for _, line := range strings.Split(run(project, "--listFilesOnly"), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, root+string(filepath.Separator)) {
				files = append(files, filepath.ToSlash(strings.TrimPrefix(line, root+string(filepath.Separator))))
			}
		}
		slices.Sort(files)
		return files
	}
	actual, reference := listedRoots(out), listedRoots("pkg/tsconfig.json")
	t.Logf("action roots=%v; authored roots=%v", actual, reference)
	if !reflect.DeepEqual(actual, reference) {
		t.Fatalf("action roots %v differ from compiler reference %v", actual, reference)
	}
	for _, required := range []string{"pkg/src/main.ts", "pkg/src/view.tsx", "pkg/src/value.d.mts", "pkg/src/explicit.ts"} {
		if !slices.Contains(actual, required) {
			t.Fatalf("missing root %s: %v", required, actual)
		}
	}
	for _, excluded := range []string{"pkg/src/value.mjs", "pkg/src/excluded.ts"} {
		if slices.Contains(actual, excluded) {
			t.Fatalf("unexpected root %s: %v", excluded, actual)
		}
	}
	run("pkg/tsconfig.json", "--noEmit")
	run(out, "--noEmit")
	assertJSON(t, "compiler-resolved emitter options", readJSON(t, binDir+"/pkg/options.json"), `{"target":"es2017","module":"preserve","jsx":"preserve"}`)
	writeFile(t, "pkg/nested/tsconfig.json", `{"compilerOptions":{"target":"es5","types":["missing-type"]}}`)
	var preserved = map[string][]byte{}
	var sources []string
	err = filepath.WalkDir(root, func(at string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, at)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(at)
		if err != nil {
			return err
		}
		preserved[rel] = body
		if !strings.HasPrefix(rel, "bazel-out/") {
			sources = append(sources, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"pkg/tsconfig.json", "pkg/nested/tsconfig.json"} {
		args := []string{"-root=" + programRoot, "-discover-tsconfig=" + out}
		for _, source := range sources {
			args = append(args, "-source="+source)
		}
		args = append(args, "--", compiler, "--project", project, "--noEmit", "--pretty", "false")
		if err := runTsgo(args); err != nil {
			t.Fatalf("actual compiler through lint discovery %s: %v", project, err)
		}
		if _, err := os.Stat(programRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging survived: %v", err)
		}
	}
	for file, before := range preserved {
		after, err := os.ReadFile(file)
		if err != nil || string(after) != string(before) {
			t.Fatalf("discovery changed canonical input %s: %v", file, err)
		}
	}
	t.Log("actual compiler accepted own and nested lint discovery; canonical inputs unchanged")

}
