// Package dev_server_test runs the launcher as `bazel run` does against a
// throwaway workspace and asserts over HTTP; the env picks the variant.
package dev_server_test

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mikn/rules_typescript/tests/hmrsocket"
	"github.com/mikn/rules_typescript/tests/verify"
)

func TestDevServerBehaviour(t *testing.T) {
	started := time.Now()
	tree := verify.New(t)
	target := env(t, "DEV_TARGET")
	wantPort := env(t, "DEV_PORT")
	wantBazelPlugin := env(t, "DEV_BAZEL_PLUGIN") == "1"
	wantReactRefresh := env(t, "DEV_REACT_REFRESH") == "1"
	wantUserConfig := env(t, "DEV_USER_CONFIG") == "1"
	impl := env(t, "DEV_IMPL")

	node := tree.File("ts/toolchain/node_resolved/node")
	launcher := tree.File("tests/dev_server/" + target + "_launcher")
	config := tree.File("tests/dev_server/" + target + "_dev/vite.config.mjs")
	readConfig := tree.File("tests/dev_server/read_config.mjs")
	found := true
	for _, f := range []verify.File{node, launcher, config, readConfig} {
		if !f.Exists() {
			found = false
		}
	}
	if !found {
		t.FailNow()
	}

	// app.ts and bazel-bin/app.js carry different marker strings, which is how
	// the response tells us which of the two the server chose.
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	if wantBazelPlugin && impl != "oj" {
		physicalWorkspace := filepath.Join(tmp, "physical-ws")
		mkdir(t, physicalWorkspace)
		if err := os.Symlink(physicalWorkspace, ws); err != nil {
			t.Fatal(err)
		}
	}
	appRoot := filepath.Join(ws, "tests", "dev_server")
	mkdir(t, appRoot)
	bazelBin := filepath.Join(ws, "bazel-bin")
	appBin := filepath.Join(bazelBin, "tests", "dev_server")
	mkdir(t, appBin)
	if target == "dev_oj_source" {
		for _, rel := range []string{"dev_app.ts", "lib/index.ts"} {
			content, err := os.ReadFile(tree.File("tests/dev_server/" + rel).Abs())
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(appRoot, rel)
			mkdir(t, filepath.Dir(dest))
			write(t, dest, string(content))
		}
	}
	write(t, filepath.Join(ws, "index.html"), "WORKSPACE_INDEX")
	write(t, filepath.Join(appRoot, "index.html"), `<html>NESTED_APP_INDEX<script type="module" src="/entry.js"></script></html>`)
	write(t, filepath.Join(appRoot, "app.ts"), "export const origin: string = \"TS_SOURCE_TRANSFORMED_BY_VITE\";\n")
	write(t, filepath.Join(appBin, "app.js"), "export const origin = \"JS_PRECOMPILED_BY_BAZEL\";\n")
	write(t, filepath.Join(ws, "tests", "shared.ts"), `export const shared = "SIBLING_SOURCE";`)
	write(t, filepath.Join(appRoot, "sibling_entry.js"), `import { shared } from "../shared.ts"; export { shared };`)
	blocked := filepath.Join(ws, "tests", "blocked.ts")
	defaultDenied := filepath.Join(ws, "tests", ".env")
	if impl == "oj" {
		write(t, blocked, `export const blocked = "DENIED_FIXTURE";`)
		write(t, defaultDenied, "FIXTURE_ONLY=true")
		canonicalBlocked, err := filepath.EvalSymlinks(blocked)
		if err != nil {
			t.Fatal(err)
		}
		canonicalWorkspace, err := filepath.EvalSymlinks(ws)
		if err != nil {
			t.Fatal(err)
		}
		nativeConfig, err := json.Marshal(map[string]any{
			"server": map[string]any{"fs": map[string]any{
				"allow": []string{canonicalWorkspace},
				"deny":  []string{filepath.ToSlash(canonicalBlocked)},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(appRoot, "oj.config.json"), string(nativeConfig))
	}
	write(t, filepath.Join(appRoot, "entry.js"), "import { origin } from \"./app.ts\";\nexport { origin };\n")
	// No JSX: what is under test is the Fast Refresh transform, and JSX would pull
	// in react/jsx-runtime, which needs a react this npm tree does not carry.
	write(t, filepath.Join(appRoot, "widget.tsx"), "export function Widget() {\n  return null;\n}\n")

	// A bare npm specifier, and one for a package no tree has.
	write(t, filepath.Join(appRoot, "npm_entry.js"), "import { z } from \"zod\";\nexport { z };\n")
	write(t, filepath.Join(appRoot, "npm_missing.js"),
		"import x from \"not-in-any-npm-tree\";\nexport { x };\n")

	// A ts_codegen output: generated .ts under bazel-bin with no source in the
	// workspace. Vite cannot produce it, so this is what bazel-bin is still for.
	generated := filepath.Join(appBin, "generated", "routes.ts")
	mkdir(t, filepath.Dir(generated))
	write(t, generated, "export const routes: string[] = [\"CODEGEN_V1\"];\n")
	write(t, filepath.Join(appRoot, "gen_entry.js"),
		"import { routes } from \"./generated/routes.ts\";\nexport { routes };\n")

	if target == "dev_with_plugin" {
		compiledTree := "tests/codegen_tree/compiled"
		copyResolved(t, filepath.Join(bazelBin, filepath.FromSlash(compiledTree)), tree.Path(compiledTree))
		copyResolved(t, filepath.Join(appBin, "generated_package"), tree.Path("tests/dev_server/generated_package"))
		write(t, filepath.Join(appBin, "outside.js"), `export const marker = "OUTSIDE_DECLARED_TREE";`)
		write(t, filepath.Join(appBin, "generated_package.js"), `export const marker = "OUTSIDE_DECLARED_TREE";`)
		for _, relative := range []string{"index.js", "dist/client.js", "dist/extension.mjs"} {
			shadow := filepath.Join(appRoot, "generated_package", relative)
			mkdir(t, filepath.Dir(shadow))
			write(t, shadow, `export const marker = "CHECKOUT_PACKAGE_SHADOW";`)
		}
		for _, relative := range []string{"index.js", "index.ts", "messages/greeting.js", "messages/greeting.ts"} {
			shadow := filepath.Join(ws, filepath.FromSlash(compiledTree), filepath.FromSlash(relative))
			mkdir(t, filepath.Dir(shadow))
			write(t, shadow, `export const greeting = () => "STALE_CHECKOUT_TREE";`)
		}
		for _, relative := range []string{"declared_source/value.ts", "declared_source/value.json", "lib/index.ts"} {
			root := appBin
			if relative == "lib/index.ts" {
				root = appRoot
			}
			destination := filepath.Join(root, filepath.FromSlash(relative))
			mkdir(t, filepath.Dir(destination))
			write(t, destination, tree.File("tests/dev_server/"+relative).Text())
		}
		mkdir(t, filepath.Join(appRoot, "declared_source"))
		write(t, filepath.Join(appRoot, "declared_source", "value.ts"), `export const generated = "STALE_CHECKOUT_TS";`)
		write(t, filepath.Join(appRoot, "declared_source", "value.json"), `{"value":"STALE_CHECKOUT_JSON"}`)
		for _, extension := range []string{"tsx", "js", "mjs"} {
			name := "extension-" + extension
			write(t, filepath.Join(appBin, "declared_source", name+"."+extension), tree.File("tests/dev_server/declared_source/"+name+"."+extension).Text())
			write(t, filepath.Join(appRoot, "declared_source", name+".ts"), `export const marker = "UNDECLARED_EXTENSION_SHADOW";`)
		}
		write(t, filepath.Join(appRoot, "declared_source", "extension-tsx.jsx"), `export const marker = "UNDECLARED_JSX_SHADOW";`)
		for _, relative := range []string{"declared_source/missing.tsx", "declared_source/missing.ts", "declared_source/missing.jsx", "generated_package/dist/missing.js", "declared_missing.css"} {
			shadow := filepath.Join(appRoot, filepath.FromSlash(relative))
			mkdir(t, filepath.Dir(shadow))
			write(t, shadow, `export const marker = "UNDECLARED_MISSING_PRODUCER_SHADOW";`)
		}
		write(t, filepath.Join(appRoot, "theme.css"), ".UNDECLARED_ASSET_SHADOW { color: blue; }")
		for _, publisher := range []string{"tests/dev_server", "tests/dev_server/lib"} {
			for _, relative := range []string{"settings.mjs", "theme.css", "tests/dev_server_assets/helper.mjs", "tests/dev_server_assets/style.css", "tests/dev_server_assets/nested.css", "tests/dev_server_assets/payload.svg"} {
				logical := publisher + "/" + relative
				published := filepath.Join(bazelBin, filepath.FromSlash(logical))
				mkdir(t, filepath.Dir(published))
				write(t, published, tree.File(logical).Text())
			}
		}
		for _, relative := range []string{"helper.mjs", "style.css", "nested.css", "payload.svg"} {
			shadow := filepath.Join(ws, "tests", "dev_server_assets", relative)
			mkdir(t, filepath.Dir(shadow))
			write(t, shadow, "UNDECLARED_CHECKOUT_BYTES")
		}
	}

	// The config watches its own bazel-bin copy, so the scratch workspace needs
	// one for the restart decision to have a baseline to move away from.
	scratchConfig := filepath.Join(bazelBin, "tests", "dev_server", target+"_dev", "vite.config.mjs")
	mkdir(t, filepath.Dir(scratchConfig))
	write(t, scratchConfig, "// stand-in for the generated config, v1\n")

	// ── 1: what the config says, by running it ────────────────────────────────
	cfg := evalConfig(t, node.Abs(), readConfig.Abs(), config.Abs(),
		readLauncherConfig(t, tree, target).env(tree, ws))
	t.Logf("config = %s", cfg.raw)

	if got := strconv.Itoa(cfg.Port); got != wantPort {
		t.Errorf("the config serves port %s, but the rule was given %s", got, wantPort)
	}
	if cfg.Root != appRoot {
		t.Errorf("config root is %s, want the application root %s", cfg.Root, appRoot)
	}
	if !slices.Contains(cfg.FsAllow, bazelBin) {
		t.Errorf("server.fs.allow does not include %s: %v", bazelBin, cfg.FsAllow)
	}

	// Restart-or-keep, as the config declares it: the config itself is fixable by
	// an in-process restart, a new Vite or a new node binary is not, and no
	// ts_codegen output is on the list at all -- that is the whole point.
	wantInputs := map[string]string{scratchConfig: "restart"}
	for _, input := range cfg.ConfigInputs {
		if want, ok := wantInputs[input.Path]; ok {
			if input.Remedy != want {
				t.Errorf("input %s declares remedy %q, want %q", input.Path, input.Remedy, want)
			}
			delete(wantInputs, input.Path)
		}
		if strings.HasPrefix(input.Path, filepath.Join(appBin, "generated")) {
			t.Errorf("a ts_codegen output is a watched config input (%s); a codegen "+
				"rebuild would restart the dev server", input.Path)
		}
	}
	for path := range wantInputs {
		t.Errorf("configInputs does not watch %s: %v", path, cfg.ConfigInputs)
	}

	// npm resolution is a plugin, because Vite has no option for it: resolve.modules
	// is webpack's, and a config that sets it configures nothing.
	if !slices.Contains(cfg.Plugins, "bazel:npm-resolve") {
		t.Errorf("the config installs no bazel:npm-resolve plugin, so no bare npm "+
			"specifier can resolve: plugins = %v", cfg.Plugins)
	}
	if cfg.ResolveModules != nil {
		t.Errorf("the config sets resolve.modules = %v, which is a webpack option Vite "+
			"ignores; whatever it was meant to do is not being done", cfg.ResolveModules)
	}
	if got := slices.Contains(cfg.Plugins, "vite:react-babel"); got != wantReactRefresh {
		t.Errorf("react plugin present = %v, want %v: plugins = %v",
			got, wantReactRefresh, cfg.Plugins)
	}
	// A framework transform has to see a module before the Bazel plugins do.
	if wantUserConfig {
		if len(cfg.Plugins) == 0 || cfg.Plugins[0] != "devserver-user-config" {
			t.Errorf("the vite_config plugin is not first in the container: plugins = %v",
				cfg.Plugins)
		}
	}

	// Args past the launcher reach the server's own CLI, which is not a shared
	// surface: --strictPort is Vite's.
	var extraArgs []string
	if impl == "vite" {
		extraArgs = append(extraArgs, "--strictPort")
	}
	srv := start(t, launcher.Abs(), ws, tmp, extraArgs...)
	base := srv.awaitHTTP(t, "/app.ts")
	t.Logf("%s (%s) is up and answering on %s", target, impl, base)

	if target == "dev_with_plugin" {
		deadline, ok := t.Deadline()
		if !ok {
			seconds, err := strconv.Atoi(os.Getenv("TEST_TIMEOUT"))
			if err != nil || seconds <= 0 {
				t.Fatal("Bazel test timeout is unavailable")
			}
			deadline = started.Add(time.Duration(seconds) * time.Second)
		}
		if inherited := os.Getenv("FORMAL_OWNER_DEADLINE_UNIX_NS"); inherited != "" {
			nanos, err := strconv.ParseInt(inherited, 10, 64)
			if err != nil || nanos <= 0 {
				t.Fatal("enclosing qualification deadline is invalid")
			}
			if enclosing := time.Unix(0, nanos); enclosing.Before(deadline) {
				deadline = enclosing
			}
		}
		t.Run("transitioned_source_cannot_select_server_configuration", func(t *testing.T) {
			var layout struct {
				Logical   string
				Producer  string
				Server    string
				ServerBin string `json:"server_bin"`
			}
			tree.File("tests/dev_server/transitioned_source.json").JSON(&layout)
			if layout.Producer == layout.Server {
				t.Fatal("fixture producer did not transition to another configuration")
			}
			producer := tree.File(layout.Logical)
			twin := tree.File("tests/dev_server/transitioned_source.server.ts")
			if producer.Text() == twin.Text() {
				t.Fatal("fixture configurations have identical source bytes")
			}
			tmp := t.TempDir()
			ws := filepath.Join(tmp, "ws")
			app := filepath.Join(ws, "tests", "dev_server")
			execRoot := filepath.Join(tmp, "execroot")
			serverBin := filepath.Join(execRoot, filepath.FromSlash(layout.ServerBin))
			mkdir(t, app)
			mkdir(t, serverBin)
			if err := os.Symlink(serverBin, filepath.Join(ws, "bazel-bin")); err != nil {
				t.Fatal(err)
			}
			for _, input := range []struct {
				path string
				file verify.File
			}{
				{layout.Producer, producer},
				{layout.Server, twin},
			} {
				destination := filepath.Join(execRoot, filepath.FromSlash(input.path))
				mkdir(t, filepath.Dir(destination))
				if err := os.Symlink(input.file.Abs(), destination); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(app, "index.html"), "CONFIGURATION_FIXTURE")
			write(t, filepath.Join(app, "configuration_entry.js"), `export { configuration } from "./configuration/value.js";`)
			srv := start(t, tree.File("tests/dev_server/dev_configuration_launcher").Abs(), ws, tmp)
			base := srv.awaitHTTP(t, "/index.html")
			entry := get(t, base, "/configuration_entry.js")
			if entry.status != 200 {
				t.Fatalf("configuration entry returned HTTP %d: %s\n%s", entry.status, entry.body, srv.log(t))
			}
			module := get(t, base, importURL(t, entry.body, "value.ts", ""))
			if module.status != 200 {
				t.Fatalf("transitioned source returned HTTP %d: %s\n%s", module.status, module.body, srv.log(t))
			}
			module.contains(t, srv, layout.Producer)
			module.excludes(t, layout.Server, ": string")
		})
		t.Run("inherited_companion_binding_remains_runtime_reachable", func(t *testing.T) {
			const pkg = "tests/npm/dev_inherited_types"
			var previous launcherConfig
			tree.File(pkg + "/previous_member_dev_launcher.json").JSON(&previous)
			originalMember := tree.File(pkg + "/node_modules/shared/package.json").Text()
			member, err := os.ReadFile(filepath.Join(inTree(tree, previous.DevServer.NodeModules), "shared", "package.json"))
			if err != nil || string(member) != originalMember {
				t.Fatalf("prior npm_files member was replaced or its link was rebased: %v, %s", err, member)
			}
			tmp := t.TempDir()
			ws := filepath.Join(tmp, "ws")
			app := filepath.Join(ws, filepath.FromSlash(pkg))
			mkdir(t, app)
			mkdir(t, filepath.Join(ws, "bazel-bin"))
			entry := filepath.Join(app, "entry.ts")
			write(t, entry, tree.File(pkg+"/entry.ts").Text())
			write(t, filepath.Join(app, "index.html"), `<html>INHERITED_NPM_ENTRY<script type="module" src="/entry.ts"></script></html>`)
			var cfg launcherConfig
			tree.File(pkg + "/dev_launcher.json").JSON(&cfg)
			_, viewSuffix, _ := strings.Cut(cfg.DevServer.NodeModules, "/")
			tree.File(pkg + "/ordinary/" + viewSuffix + "/@types/culori").Contains("DECLARED_ORDINARY_DATA")
			var expected struct{ Name, Version string }
			data, err := os.ReadFile(filepath.Join(inTree(tree, cfg.DevServer.NodeModules), "@types", "culori", "package.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &expected); err != nil || expected.Name != "@types/culori" || expected.Version == "" {
				t.Fatalf("inherited companion metadata is unavailable: %v", err)
			}
			srv := start(t, tree.File(pkg+"/dev_launcher").Abs(), ws, tmp)
			base := srv.awaitHTTP(t, "/index.html")
			served := func(path string) response {
				t.Helper()
				got := get(t, base, path)
				if got.status != 200 {
					t.Fatalf("inherited binding returned HTTP %d for %s: %s\n%s", got.status, path, got.body, srv.log(t))
				}
				return got
			}
			module := served("/entry.ts")
			dependency := depURL(module.body)
			if !strings.Contains(dependency, "/deps/") {
				t.Fatalf("forced culori optimizer did not resolve through the app importer: %s\n%s", module.body, srv.log(t))
			}
			served(dependency)
			if strings.Contains(srv.log(t), "Failed to resolve dependency") {
				t.Fatalf("forced optimizer lost inherited npm context: %s", srv.log(t))
			}
			sock, err := hmrsocket.Dial(strings.TrimPrefix(base, "http://"), "/", "vite-hmr")
			if err != nil {
				t.Fatal(err)
			}
			defer sock.Close()
			updated := `import metadata from "@types/culori/package.json";
export { parse } from "culori";
export const companion = metadata.name;
`
			write(t, entry, updated)
			// Vite names the module it resolved, past every symlinked ancestor.
			resolvedEntry, err := filepath.EvalSymlinks(entry)
			if err != nil {
				t.Fatal(err)
			}
			for {
				frame, err := sock.Next(time.Until(deadline))
				if err != nil {
					t.Fatalf("inherited source edit did not refresh: %v\n%s", err, srv.log(t))
				}
				var message struct {
					Type        string `json:"type"`
					TriggeredBy string `json:"triggeredBy"`
				}
				if err := json.Unmarshal([]byte(frame), &message); err != nil {
					t.Fatal(err)
				}
				if message.Type == "error" {
					t.Fatalf("inherited source edit failed: %s", frame)
				}
				if message.Type == "full-reload" && filepath.Clean(filepath.FromSlash(message.TriggeredBy)) == resolvedEntry {
					break
				}
			}
			module = served("/entry.ts")
			metadata := served(importURL(t, module.body, "package.json", ""))
			for _, value := range []string{expected.Name, expected.Version} {
				if !strings.Contains(metadata.body, strconv.Quote(value)) {
					t.Fatalf("live inherited metadata request lost %q: %s", value, metadata.body)
				}
			}
			if got, err := os.ReadFile(entry); err != nil || string(got) != updated {
				t.Fatalf("server changed the edited source: %v", err)
			}
			t.Log("inherited companion served after live edit without rebuilding")
		})
		for _, test := range []struct {
			name, context  string
			checkoutHelper bool
		}{
			{"generated_package_import_cannot_require_checkout_target", "scope", false},
			{"generated_package_import_cannot_select_checkout_target", "scope", true},
			{"nested_same_store_preserves_declared_npm_identity", "same_store", false},
			{"nested_mirrored_store_preserves_declared_npm_identity", "mirrored_store", false},
			{"nested_install_cannot_override_declared_npm_identity", "different_store", false},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := "dev_source_contexts"
				tmp := t.TempDir()
				ws := filepath.Join(tmp, "ws")
				app := filepath.Join(ws, "tests", "dev_server")
				bin := filepath.Join(ws, "bazel-bin", "tests", "dev_server")
				for _, dir := range []string{app, filepath.Join(app, "scoped"), filepath.Join(app, "npm_context"), filepath.Join(bin, "scoped")} {
					mkdir(t, dir)
				}
				for _, rel := range []string{"scoped/package.json", "npm_context/value.ts"} {
					write(t, filepath.Join(app, rel), tree.File("tests/dev_server/"+rel).Text())
				}
				write(t, filepath.Join(app, "scoped", "helper.mjs"), tree.File("tests/dev_server/scoped/helper.mjs").Text()+`
export { default as generatedSource } from "#generated-helper?raw";
`)
				for _, name := range []string{"generated.ts", "generated-helper.ts", "self-helper.ts"} {
					generated := tree.File("tests/dev_server/scoped/" + name).Abs()
					selected := filepath.Join(bin, "scoped", name)
					if err := os.Symlink(generated, selected); err != nil {
						t.Fatal(err)
					}
					originalInfo, err := os.Stat(generated)
					if err != nil {
						t.Fatal(err)
					}
					selectedInfo, err := os.Stat(selected)
					if err != nil || !os.SameFile(originalInfo, selectedInfo) {
						t.Fatalf("generated input %s lost its exact Bazel File: %v", name, err)
					}
				}
				for _, absent := range []string{filepath.Join(bin, "scoped", "package.json"), filepath.Join(bin, "scoped", "generated.js")} {
					if _, err := os.Lstat(absent); !os.IsNotExist(err) {
						t.Fatalf("source-mode fixture unexpectedly staged %s: %v", absent, err)
					}
				}
				tree.Absent("tests/dev_server/scoped/generated.js")
				checkoutTwin := filepath.Join(app, "scoped", "generated.ts")
				poison := `export const generated = "POISONED_CHECKOUT_GENERATED"; export const helper = "POISONED_CHECKOUT_HELPER";`
				write(t, checkoutTwin, poison)
				write(t, filepath.Join(app, "index.html"), "SOURCE_CONTEXT_FIXTURE")
				entry := `import { generated, generatedHelper, helper } from "./scoped/generated.js"; export { generated, generatedHelper, helper };`
				preserved := map[string]string{checkoutTwin: poison}
				helperTwin := filepath.Join(app, "scoped", "generated-helper.ts")
				if test.context == "scope" {
					installed := filepath.Join(bin, "scoped", "node_modules", "@fixture", "declared-scope")
					mkdir(t, installed)
					preserved[filepath.Join(installed, "package.json")] = `{"name":"@fixture/declared-scope","type":"module","exports":{"./self-helper":"./helper.mjs"}}`
					preserved[filepath.Join(installed, "helper.mjs")] = `export const selfHelper = "INSTALLED_PACKAGE_SHADOW";`
					for file, content := range preserved {
						write(t, file, content)
					}
				}
				if test.checkoutHelper {
					preserved[helperTwin] = `export const generatedHelper = "POISONED_CHECKOUT_GENERATED_HELPER";`
					write(t, helperTwin, preserved[helperTwin])
				} else if _, err := os.Lstat(helperTwin); !os.IsNotExist(err) {
					t.Fatalf("generated helper unexpectedly has a checkout target: %v", err)
				}
				var nestedModules, declaredModules, declaredVersion string
				if test.context != "scope" {
					entry = `import { version } from "./npm_context/value.js"; export { version };`
					cfg := readLauncherConfig(t, tree, fixture)
					declaredModules = inTree(tree, cfg.DevServer.NodeModules)
					var metadata struct{ Version string }
					data, err := os.ReadFile(filepath.Join(declaredModules, "zod", "package.json"))
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &metadata); err != nil || metadata.Version == "" {
						t.Fatalf("declared npm package has no version: %v", err)
					}
					declaredVersion = metadata.Version
					nestedModules = filepath.Join(app, "npm_context", "node_modules")
					if test.context == "same_store" {
						if err := os.Symlink(declaredModules, nestedModules); err != nil {
							t.Fatal(err)
						}
					} else if test.context == "mirrored_store" {
						// The darwin-sandbox shape: a real directory of links to the declared store's files.
						store, err := filepath.EvalSymlinks(filepath.Join(declaredModules, "zod"))
						if err != nil {
							t.Fatal(err)
						}
						if err := filepath.WalkDir(store, func(file string, entry os.DirEntry, err error) error {
							if err != nil {
								return err
							}
							relative, err := filepath.Rel(store, file)
							if err != nil {
								return err
							}
							mirrored := filepath.Join(nestedModules, "zod", relative)
							if entry.IsDir() {
								return os.MkdirAll(mirrored, 0o755)
							}
							return os.Symlink(file, mirrored)
						}); err != nil {
							t.Fatal(err)
						}
					} else {
						mkdir(t, filepath.Join(nestedModules, "zod"))
						preserved[filepath.Join(nestedModules, "zod", "package.json")] = `{"name":"zod","version":"0.0.0-NESTED_CHECKOUT_OVERRIDE","type":"module","exports":{".":"./index.js","./package.json":"./package.json"}}`
						preserved[filepath.Join(nestedModules, "zod", "index.js")] = `export const z = "NESTED_CHECKOUT_OVERRIDE";`
						preserved[filepath.Join(nestedModules, ".user-owned")] = "KEEP_USER_INSTALL"
						for file, content := range preserved {
							write(t, file, content)
						}
					}
				}
				write(t, filepath.Join(app, "context_entry.js"), entry)
				t.Cleanup(func() {
					for file, want := range preserved {
						got, err := os.ReadFile(file)
						if err != nil || string(got) != want {
							t.Errorf("dev server changed user file %s: %v; got %q", file, err, got)
						}
					}
					if !test.checkoutHelper {
						if _, err := os.Lstat(helperTwin); !os.IsNotExist(err) {
							t.Errorf("dev server materialized the generated helper in the checkout: %v", err)
						}
					}
					if test.context == "same_store" {
						if got, err := os.Readlink(nestedModules); err != nil || got != declaredModules {
							t.Errorf("dev server replaced the user's same-store link: %q (%v)", got, err)
						}
					}
				})
				srv := start(t, tree.File("tests/dev_server/"+fixture+"_launcher").Abs(), ws, tmp)
				if test.context == "different_store" {
					timer := time.NewTimer(time.Until(deadline))
					defer timer.Stop()
					select {
					case err := <-srv.wait:
						srv.wait <- err
						if err == nil {
							t.Fatal("conflicting npm installation exited successfully")
						}
					case <-timer.C:
						t.Fatal("conflicting npm installation was not refused before the test deadline")
					}
					log := srv.log(t)
					for _, want := range []string{"conflicting npm installation for zod", nestedModules, "declared app store", "Remove the conflicting installation yourself"} {
						if !strings.Contains(log, want) {
							t.Errorf("npm startup refusal lacks %q:\n%s", want, log)
						}
					}
					return
				}
				base := srv.awaitHTTP(t, "/index.html")
				get(t, base, "/index.html").contains(t, srv, "SOURCE_CONTEXT_FIXTURE")
				t.Logf("declared source context %s reached serving", test.context)
				served := func(path string) response {
					t.Helper()
					got := get(t, base, path)
					if got.status != 200 {
						t.Fatalf("declared source context %s returned HTTP %d for %s: %s\n%s", test.context, got.status, path, got.body, srv.log(t))
					}
					return got
				}
				response := served("/context_entry.js")
				if test.context == "scope" {
					generatedURL := importURL(t, response.body, "generated.ts", "")
					module := get(t, base, generatedURL)
					if module.status != 200 {
						t.Fatalf("generated package #imports lost authored scope: HTTP %d at %s\n%s\n%s", module.status, generatedURL, module.body, srv.log(t))
					}
					module.contains(t, srv, "DECLARED_SCOPED_GENERATED")
					module.excludes(t, "POISONED_CHECKOUT_GENERATED", "INSTALLED_PACKAGE_SHADOW", "node_modules/@fixture", ": string")
					module.contains(t, srv, "selfHelper")
					self := served(importURL(t, module.body, "self-helper.ts", ""))
					self.contains(t, srv, "DECLARED_SELF_REFERENCE_HELPER")
					self.excludes(t, "INSTALLED_PACKAGE_SHADOW", ": string")
					helper := served(importURL(t, module.body, "helper.mjs", ""))
					helper.contains(t, srv, "AUTHORED_SCOPE_HELPER")
					helper.excludes(t, "POISONED_CHECKOUT_HELPER")
					for _, request := range []struct{ body, query string }{
						{module.body, ""},
						{helper.body, "raw"},
					} {
						generatedHelper := served(importURL(t, request.body, "generated-helper.ts", request.query))
						generatedHelper.contains(t, srv, "DECLARED_GENERATED_SCOPE_HELPER")
						generatedHelper.excludes(t, "POISONED_CHECKOUT_GENERATED_HELPER")
						if request.query == "raw" {
							generatedHelper.contains(t, srv, ": string")
						} else {
							generatedHelper.excludes(t, ": string")
						}
					}
					return
				}
				module := served(importURL(t, response.body, "value.ts", ""))
				metadata := served(importURL(t, module.body, "package.json", ""))
				if strings.Contains(metadata.body, "NESTED_CHECKOUT_OVERRIDE") || !strings.Contains(metadata.body, strconv.Quote(declaredVersion)) {
					t.Fatalf("nested installation changed declared npm identity: want zod %q, served %s\n%s", declaredVersion, metadata.finalURL, metadata.body)
				}
			})
		}
		t.Run("shared_backing_scalars_cannot_replace_selected_scope", func(t *testing.T) {
			tmp := t.TempDir()
			ws := filepath.Join(tmp, "ws")
			app := filepath.Join(ws, "tests", "dev_server")
			bin := filepath.Join(ws, "bazel-bin", "tests", "dev_server", "shared_scalars")
			backing := tree.File("tests/dev_server/shared_scalars/a/value.ts")
			if backing.Text() != tree.File("tests/dev_server/shared_scalars/b/value.ts").Text() {
				t.Fatal("shared-backing fixture has different declared source bytes")
			}
			backingInfo, err := os.Stat(backing.Abs())
			if err != nil {
				t.Fatal(err)
			}
			for _, side := range []string{"a", "b"} {
				mkdir(t, filepath.Join(bin, side))
				for _, name := range []string{"value.ts", "helper.ts", "scoped-helper.ts", "imports.ts", "self.ts", "package.json"} {
					source := tree.File("tests/dev_server/shared_scalars/" + side + "/" + name)
					if name == "value.ts" {
						source = backing
					}
					if err := os.Symlink(source.Abs(), filepath.Join(bin, side, name)); err != nil {
						t.Fatal(err)
					}
				}
				selectedInfo, err := os.Stat(filepath.Join(bin, side, "value.ts"))
				if err != nil || !os.SameFile(backingInfo, selectedInfo) {
					t.Fatalf("declared scalar %s does not share its backing File: %v", side, err)
				}
			}
			// Declared outputs are addressed beneath the resolved bazel-bin.
			resolvedBin, err := filepath.EvalSymlinks(bin)
			if err != nil {
				t.Fatal(err)
			}
			mkdir(t, app)
			write(t, filepath.Join(app, "index.html"), "SHARED_SCALAR_FIXTURE")
			srv := start(t, tree.File("tests/dev_server/dev_source_contexts_launcher").Abs(), ws, tmp)
			base := srv.awaitHTTP(t, "/index.html")
			served := func(t *testing.T, request string) response {
				t.Helper()
				got := get(t, base, request)
				if got.status != 200 {
					t.Fatalf("shared scalar returned HTTP %d for %s: %s\n%s", got.status, request, got.body, srv.log(t))
				}
				return got
			}
			for _, side := range []string{"b", "a"} {
				other := "a"
				if side == "a" {
					other = "b"
				}
				for _, mode := range []string{"imports", "self"} {
					t.Run(side+"_"+mode, func(t *testing.T) {
						entry := served(t, "/@fs"+filepath.ToSlash(filepath.Join(bin, side, mode+".ts")))
						valueURL := importURL(t, entry.body, "value.ts", "")
						selected, err := url.Parse(valueURL)
						want := "/@fs" + filepath.ToSlash(filepath.Join(resolvedBin, side, "value.ts"))
						if err != nil || selected.Path != want {
							t.Fatalf("%s selected %q, want declared scalar %q: %v", mode, valueURL, want, err)
						}
						value := served(t, valueURL)
						for _, helper := range []struct{ file, marker string }{
							{"helper.ts", "DECLARED_RELATIVE_"},
							{"scoped-helper.ts", "DECLARED_SCOPE_"},
						} {
							response := served(t, importURL(t, value.body, helper.file, ""))
							response.contains(t, srv, helper.marker+strings.ToUpper(side))
							response.excludes(t, helper.marker+strings.ToUpper(other))
						}
					})
				}
			}
		})
		t.Run("generated_tree_uses_package_main_and_vite_extensions", func(t *testing.T) {
			for _, test := range []struct{ name, specifier, file, marker string }{
				{"package_main", "./generated_package", "client.js", "DECLARED_PACKAGE_MAIN"},
				{"extensionless_mjs", "./generated_package/dist/extension", "extension.mjs", "DECLARED_EXTENSIONLESS_MJS"},
			} {
				t.Run(test.name, func(t *testing.T) {
					entry := "/generated_package_" + test.name + ".js"
					write(t, filepath.Join(appRoot, strings.TrimPrefix(entry, "/")), "export { marker } from "+strconv.Quote(test.specifier)+";\n")
					module := get(t, base, entry)
					if module.status != 200 {
						t.Fatalf("generated package entry returned HTTP %d: %s\n%s", module.status, module.body, srv.log(t))
					}
					selected := get(t, base, importURL(t, module.body, test.file, ""))
					selected.contains(t, srv, test.marker)
					selected.excludes(t, "CHECKOUT_PACKAGE_SHADOW", "WRONG_GENERATED_INDEX", "OUTSIDE_DECLARED_TREE")
				})
			}
			t.Run("package_main_cannot_escape_declared_tree", func(t *testing.T) {
				write(t, filepath.Join(appRoot, "generated_package_escape.js"), `export { marker } from "./generated_package/escape";`)
				module := get(t, base, "/generated_package_escape.js")
				if module.status == 200 || !strings.Contains(module.body, "outside declared directory") {
					t.Fatalf("escaping package main was not rejected: HTTP %d: %s\n%s", module.status, module.body, srv.log(t))
				}
			})
		})
		t.Run("generated_scalars_override_checkout_extension_candidates", func(t *testing.T) {
			for _, extension := range []string{"tsx", "js", "mjs"} {
				t.Run(extension, func(t *testing.T) {
					name := "extension-" + extension
					entry := "/generated_scalar_" + extension + ".js"
					write(t, filepath.Join(appRoot, strings.TrimPrefix(entry, "/")), "export { marker } from "+strconv.Quote("./declared_source/"+name)+";\n")
					module := get(t, base, entry)
					if module.status != 200 {
						t.Fatalf("generated scalar entry returned HTTP %d: %s\n%s", module.status, module.body, srv.log(t))
					}
					selected := get(t, base, importURL(t, module.body, name+"."+extension, ""))
					selected.contains(t, srv, "DECLARED_SCALAR_"+strings.ToUpper(extension))
					selected.excludes(t, "UNDECLARED_EXTENSION_SHADOW")
				})
			}
		})
		requireMissingProducer := func(t *testing.T, got response, relative string) {
			t.Helper()
			physicalBin, err := filepath.EvalSymlinks(appBin)
			if err != nil {
				t.Fatal(err)
			}
			producer := filepath.ToSlash(filepath.Join(physicalBin, filepath.FromSlash(relative)))
			if got.status < 400 || !(strings.Contains(got.body, "ENOENT") || strings.Contains(got.body, "Failed to load url")) || !strings.Contains(got.body, producer) {
				t.Fatalf("missing producer %s did not report its filesystem error: HTTP %d: %s\n%s", producer, got.status, got.body, srv.log(t))
			}
			got.excludes(t, "UNDECLARED_MISSING_PRODUCER_SHADOW")
		}
		t.Run("root_relative_requests_retain_declared_identity", func(t *testing.T) {
			for _, test := range []struct{ name, request, marker string }{
				{"scalar", "/declared_source/value.ts", "DECLARED_GENERATED_TS_V1"},
				{"tree", "/generated_package/dist/client.js", "DECLARED_PACKAGE_MAIN"},
				{"asset", "/theme.css?raw", ".first-publisher"},
				{"missing_scalar", "/declared_source/missing.tsx", ""},
				{"missing_tree", "/generated_package/dist/missing.js", ""},
				{"missing_asset", "/declared_missing.css?raw", ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					selected := get(t, base, test.request)
					selected.excludes(t, "STALE_CHECKOUT_TS", "CHECKOUT_PACKAGE_SHADOW", "UNDECLARED_ASSET_SHADOW", "UNDECLARED_MISSING_PRODUCER_SHADOW")
					if test.marker == "" {
						requireMissingProducer(t, selected, strings.TrimPrefix(strings.Split(test.request, "?")[0], "/"))
						return
					}
					if selected.status != 200 {
						t.Fatalf("declared root request returned HTTP %d: %s\n%s", selected.status, selected.body, srv.log(t))
					}
					selected.contains(t, srv, test.marker)
				})
			}
		})
		t.Run("jsx_imports_retain_declared_typescript_identity", func(t *testing.T) {
			for _, name := range []string{"extension-tsx", "missing"} {
				t.Run(name, func(t *testing.T) {
					entry := "/generated_jsx_" + name + ".js"
					write(t, filepath.Join(appRoot, strings.TrimPrefix(entry, "/")), "export { marker } from "+strconv.Quote("./declared_source/"+name+".jsx")+";\n")
					module := get(t, base, entry)
					if name == "missing" && module.status != 200 {
						requireMissingProducer(t, module, "declared_source/missing.tsx")
						return
					}
					if module.status != 200 {
						t.Fatalf("declared JSX entry returned HTTP %d: %s\n%s", module.status, module.body, srv.log(t))
					}
					selected := get(t, base, importURL(t, module.body, name+".tsx", ""))
					selected.excludes(t, "UNDECLARED_JSX_SHADOW", "UNDECLARED_EXTENSION_SHADOW", "UNDECLARED_MISSING_PRODUCER_SHADOW")
					if name == "missing" {
						requireMissingProducer(t, selected, "declared_source/missing.tsx")
						return
					}
					if selected.status != 200 {
						t.Fatalf("declared JSX producer returned HTTP %d: %s\n%s", selected.status, selected.body, srv.log(t))
					}
					selected.contains(t, srv, "DECLARED_SCALAR_TSX")
				})
			}
		})
		t.Run("generated_sources_override_checkout_twins_and_refresh_without_emit", func(t *testing.T) {
			phase, boundary, acceptedURL, lastFrame := "load generated imports", "", "", ""
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("phase=%s boundary=%q accepted=%q last frame=%q\n%s", phase, boundary, acceptedURL, lastFrame, srv.log(t))
				}
			})
			t.Log(phase)
			served := func(path string) response {
				t.Helper()
				got := get(t, base, path)
				if got.status != 200 {
					t.Fatalf("generated request %s returned %d: %s", path, got.status, got.body)
				}
				return got
			}
			entry := "/declared_entry.js"
			write(t, filepath.Join(appRoot, "declared_entry.js"), `
import { generated, packageName } from "./declared_source/value.js";
import payload from "./declared_source/value.json";
import { greeting } from "../codegen_tree/compiled/index.js";
export { generated, packageName, payload, greeting };
if (import.meta.hot) import.meta.hot.accept(["./declared_source/value.js", "./declared_source/value.json", "../codegen_tree/compiled/index.js"], () => {});
`)
			entryResponse := served(entry)
			entry = strings.TrimPrefix(entryResponse.finalURL, base)
			sourceURL := importURL(t, entryResponse.body, "value.ts", "")
			jsonURL := importURL(t, entryResponse.body, "value.json", "")
			treeIndexURL := importURL(t, entryResponse.body, "index.js", "")
			treeIndex := served(treeIndexURL)
			treeIndex.excludes(t, "STALE_CHECKOUT_TREE")
			treeURL := importURL(t, treeIndex.body, "greeting.js", "")
			greeting := served(treeURL)
			greeting.contains(t, srv, "greeting: ")
			greeting.excludes(t, "STALE_CHECKOUT_TREE")
			accepted := regexp.MustCompile(`import\.meta\.hot\.accept\(\s*(\[[^\]]+\])`).FindStringSubmatch(entryResponse.body)
			if accepted == nil {
				t.Fatalf("generated entry has no rewritten HMR dependencies: %s", entryResponse.body)
			}
			source := served(sourceURL)
			source.contains(t, srv, "DECLARED_GENERATED_TS_V1")
			source.excludes(t, "STALE_CHECKOUT_TS", ": string")
			served(jsonURL).contains(t, srv, "DECLARED_GENERATED_JSON_V1")
			served(importURL(t, source.body, "index.ts", "")).contains(t, srv, "@devserver/lib")
			if _, err := os.Stat(filepath.Join(appBin, "declared_source", "value.js")); !os.IsNotExist(err) {
				t.Fatalf("source fixture unexpectedly has emitted JavaScript: %v", err)
			}
			phase = "connect generated HMR"
			t.Log(phase)
			// Vite replays the last error raised with no client connected, here an earlier subtest's missing producer, to the next client.
			primer, err := hmrsocket.Dial(strings.TrimPrefix(base, "http://"), "/", "vite-hmr")
			if err != nil {
				t.Fatal(err)
			}
			primer.Close()
			sock, err := hmrsocket.Dial(strings.TrimPrefix(base, "http://"), "/", "vite-hmr")
			if err != nil {
				t.Fatal(err)
			}
			defer sock.Close()
			before := restartCount(t, srv)
			for _, changed := range []struct {
				file, url, acceptedFile, before, marker string
			}{
				{"tests/dev_server/declared_source/value.ts", sourceURL, "value.ts", "DECLARED_GENERATED_TS_V1", "DECLARED_GENERATED_TS_V2"},
				{"tests/dev_server/declared_source/value.json", jsonURL, "value.json", "DECLARED_GENERATED_JSON_V1", "DECLARED_GENERATED_JSON_V2"},
				{"tests/codegen_tree/compiled/messages/greeting.js", treeURL, "index.js", "greeting: ", "TREE_GREETING_V2: "},
			} {
				phase = "replace " + changed.file
				boundary, acceptedURL = entry, importURL(t, accepted[1], changed.acceptedFile, "")
				lastFrame = ""
				t.Logf("phase=%s boundary=%q accepted=%q", phase, boundary, acceptedURL)
				original := tree.File(changed.file).Text()
				file := filepath.Join(bazelBin, filepath.FromSlash(changed.file))
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				write(t, file, strings.ReplaceAll(original, changed.before, changed.marker))
				changedFile, err := filepath.EvalSymlinks(file)
				if err != nil {
					t.Fatal(err)
				}
				phase = "await HMR for " + changed.file
				for {
					frame, err := sock.Next(time.Until(deadline))
					if err != nil {
						t.Fatalf("generated %s HMR: %v", changed.file, err)
					}
					lastFrame = frame
					t.Logf("phase=%s frame=%s", phase, frame)
					var message struct {
						Type        string `json:"type"`
						TriggeredBy string `json:"triggeredBy"`
						Updates     []struct {
							Path         string `json:"path"`
							AcceptedPath string `json:"acceptedPath"`
						} `json:"updates"`
					}
					if err := json.Unmarshal([]byte(frame), &message); err != nil {
						t.Fatal(err)
					}
					// FSEvents can deliver earlier subtests' writes late; only this file's reload counts.
					if message.Type == "full-reload" && message.TriggeredBy != changedFile {
						continue
					}
					if message.Type == "full-reload" || message.Type == "error" {
						t.Fatalf("generated source HMR boundary lost: %s", frame)
					}
					matched := false
					for _, update := range message.Updates {
						if update.Path == entry && update.AcceptedPath == acceptedURL {
							matched = true
						}
					}
					if !matched {
						continue
					}
					phase = "fetch rebuilt " + changed.file
					fresh := get(t, base, changed.url)
					if fresh.status != 200 || !strings.Contains(fresh.body, changed.marker) {
						t.Fatalf("generated %s HMR served stale bytes (%d): %s", changed.file, fresh.status, fresh.body)
					}
					break
				}
			}
			if after := restartCount(t, srv); after != before {
				t.Fatalf("generated source rebuild restarted Vite: %d -> %d", before, after)
			}
		})
		t.Run("published_assets_keep_context_bytes_and_rebuild_updates", func(t *testing.T) {
			phase, lastFrame := "native publisher imports", ""
			var pending map[string]string
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("phase=%s pending=%v last frame=%q\n%s", phase, pending, lastFrame, srv.log(t))
				}
			})
			t.Log(phase)
			served := func(url string) response {
				t.Helper()
				got := get(t, base, url)
				if got.status != 200 {
					t.Fatalf("published request %s returned %d: %s", url, got.status, got.body)
				}
				return got
			}
			publishers := []string{"tests/dev_server", "tests/dev_server/lib"}
			cmd := exec.Command(node.Abs(), "--input-type=module", "-e", `
import { pathToFileURL } from 'node:url';
const names = [];
for (const file of process.argv.slice(1)) names.push((await import(pathToFileURL(file).href)).name);
process.stdout.write(JSON.stringify(names));
`, tree.File(publishers[0]+"/tests/dev_server_assets/helper.mjs").Abs(), tree.File(publishers[1]+"/tests/dev_server_assets/helper.mjs").Abs())
			nativeJSON, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("native published modules: %v\n%s", err, nativeJSON)
			}
			var nativeNames []string
			if err := json.Unmarshal(nativeJSON, &nativeNames); err != nil || !slices.Equal(nativeNames, []string{"FIRST_PUBLISHER", "SECOND_PUBLISHER"}) {
				t.Fatalf("native publication contexts: %s (%v)", nativeJSON, err)
			}
			exportedString := func(body string) string {
				t.Helper()
				_, encoded, ok := strings.Cut(body, "export default ")
				var value string
				if !ok {
					t.Fatalf("asset module has no default export: %s", body)
				}
				if err := json.NewDecoder(strings.NewReader(encoded)).Decode(&value); err != nil {
					t.Fatalf("asset module default export: %v\n%s", err, body)
				}
				return value
			}
			assetBytes := func(value string) string {
				t.Helper()
				if strings.HasPrefix(value, "data:") {
					media, encoded, ok := strings.Cut(value, ",")
					if !ok || strings.SplitN(media, ";", 2)[0] != "data:image/svg+xml" {
						t.Fatalf("asset URL is not SVG data: %s", value)
					}
					decoded, err := url.PathUnescape(encoded)
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasSuffix(media, ";base64") {
						bytes, err := base64.StdEncoding.DecodeString(decoded)
						if err != nil {
							t.Fatal(err)
						}
						return string(bytes)
					}
					return decoded
				}
				baseURL, err := url.Parse(base)
				if err != nil {
					t.Fatal(err)
				}
				assetURL, err := baseURL.Parse(value)
				if err != nil {
					t.Fatal(err)
				}
				got := get(t, assetURL.String(), "")
				if got.status != 200 {
					t.Fatalf("asset URL %s returned %d: %s", value, got.status, got.body)
				}
				return got.body
			}
			svgTokens := func(body string) []xml.Token {
				t.Helper()
				decoder := xml.NewDecoder(strings.NewReader(strings.TrimSpace(body)))
				var tokens []xml.Token
				for {
					token, err := decoder.Token()
					if err == io.EOF {
						return tokens
					}
					if err != nil {
						t.Fatalf("invalid SVG: %v\n%s", err, body)
					}
					tokens = append(tokens, xml.CopyToken(token))
				}
			}
			var helperURLs, cssURLs []string
			for i, publisher := range publishers {
				phase = "fetch published bytes for " + publisher
				t.Log(phase)
				logical := publisher + "/tests/dev_server_assets/"
				entry := filepath.Join(appRoot, "published_"+strconv.Itoa(i)+".js")
				asset := filepath.Join(ws, filepath.FromSlash(logical+"payload.svg"))
				write(t, entry, "import { name } from "+strconv.Quote(filepath.Join(ws, filepath.FromSlash(logical+"helper.mjs")))+
					"; import "+strconv.Quote(filepath.Join(ws, filepath.FromSlash(logical+"style.css")))+
					"; import raw from "+strconv.Quote(asset+"?raw")+
					"; import assetURL from "+strconv.Quote(asset+"?url")+"; export { name, raw, assetURL };")
				entryResponse := served("/" + filepath.Base(entry))
				helperURL := importURL(t, entryResponse.body, "helper.mjs", "")
				helper := served(helperURL)
				settings := served(importURL(t, helper.body, "settings.mjs", ""))
				settings.contains(t, srv, nativeNames[i])
				helperURLs = append(helperURLs, helperURL)
				cssURL := importURL(t, entryResponse.body, "style.css", "") + "?direct"
				css := served(cssURL)
				css.contains(t, srv, []string{".first-publisher", ".second-publisher"}[i])
				cssURLs = append(cssURLs, cssURL)
				match := regexp.MustCompile(`url\(\s*(?:"([^"]*)"|'([^']*)'|([^\s)]*))\s*\)`).FindStringSubmatch(css.body)
				if match == nil {
					t.Fatalf("nested CSS has no payload URL: %s", css.body)
				}
				declared := tree.File(logical + "payload.svg").Text()
				payload := assetBytes(strings.Join(match[1:], ""))
				if !reflect.DeepEqual(svgTokens(payload), svgTokens(declared)) {
					t.Fatalf("CSS payload escaped %s: %s", logical, payload)
				}
				if raw := exportedString(served(importURL(t, entryResponse.body, "payload.svg", "raw")).body); raw != declared {
					t.Fatalf("raw asset escaped %s: %s", logical, raw)
				}
				payload = assetBytes(exportedString(served(importURL(t, entryResponse.body, "payload.svg", "url")).body))
				if !reflect.DeepEqual(svgTokens(payload), svgTokens(declared)) {
					t.Fatalf("URL asset escaped %s: %s", logical, payload)
				}
			}
			if helperURLs[0] == helperURLs[1] || cssURLs[0] == cssURLs[1] {
				t.Fatalf("publisher contexts collapsed: helpers %v; CSS %v", helperURLs, cssURLs)
			}
			phase = "connect published HMR"
			t.Log(phase)
			sock, err := hmrsocket.Dial(strings.TrimPrefix(base, "http://"), "/", "vite-hmr")
			if err != nil {
				t.Fatal(err)
			}
			defer sock.Close()
			pending = map[string]string{helperURLs[0]: "PUBLISHED_REBUILD", cssURLs[0]: "published-rebuild"}
			phase = "replace published outputs"
			t.Logf("phase=%s pending=%v", phase, pending)
			var changedFiles []string
			for relative, addition := range map[string]string{
				"helper.mjs": "\nexport const revision = 'PUBLISHED_REBUILD';\n",
				"style.css":  "\n.published-rebuild { color: purple; }\n",
			} {
				logical := publishers[0] + "/tests/dev_server_assets/" + relative
				file := filepath.Join(bazelBin, filepath.FromSlash(logical))
				bytes := tree.File(logical).Text()
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				write(t, file, bytes+addition)
				resolved, err := filepath.EvalSymlinks(file)
				if err != nil {
					t.Fatal(err)
				}
				changedFiles = append(changedFiles, resolved)
			}
			phase = "await published HMR"
			for len(pending) > 0 {
				frame, err := sock.Next(time.Until(deadline))
				if err != nil {
					t.Fatalf("published output updates %v: %v", pending, err)
				}
				lastFrame = frame
				t.Logf("phase=%s frame=%s", phase, frame)
				var message struct {
					Type        string `json:"type"`
					TriggeredBy string `json:"triggeredBy"`
					Updates     []struct {
						Path string `json:"path"`
					} `json:"updates"`
				}
				if err := json.Unmarshal([]byte(frame), &message); err != nil {
					t.Fatal(err)
				}
				// FSEvents can deliver earlier writes late; only the replaced outputs' reloads count.
				if message.Type == "full-reload" && !slices.Contains(changedFiles, message.TriggeredBy) {
					continue
				}
				if message.Type == "full-reload" || message.Type == "error" {
					t.Fatalf("published HMR boundaries lost: %s", frame)
				}
				for _, update := range message.Updates {
					if marker, ok := pending[update.Path]; ok {
						served(update.Path).contains(t, srv, marker)
						delete(pending, update.Path)
					}
				}
			}

		})
	}

	if target == "dev_oj_source" {
		t.Run("source_app_resolves_js_import_to_source_library_without_emit", func(t *testing.T) {
			app := get(t, base, "/dev_app.ts")
			if app.status != 200 {
				t.Fatalf("source app returned %d: %s", app.status, app.body)
			}
			match := regexp.MustCompile(`["']([^"']*lib/index[^"']*)["']`).FindStringSubmatch(app.body)
			if match == nil {
				t.Fatalf("source app has no library import: %s", app.body)
			}
			library := get(t, base, strings.TrimPrefix(match[1], "."))
			if library.status != 200 {
				t.Fatalf("source library returned %d: %s", library.status, library.body)
			}
			library.contains(t, srv, "@devserver/lib")
		})
	}

	if impl == "oj" {
		t.Run("application_config_denies_workspace_files", func(t *testing.T) {
			control := get(t, base, "/@fs"+filepath.Join(ws, "tests", "shared.ts"))
			if control.status != 200 {
				t.Fatalf("allowed sibling returned %d: %s", control.status, control.body)
			}
			control.contains(t, srv, "SIBLING_SOURCE")
			for _, file := range []string{blocked, defaultDenied} {
				r := get(t, base, "/@fs"+file)
				if r.status != 403 {
					t.Fatalf("GET %s returned %d, want 403: %s", file, r.status, r.body)
				}
				r.excludes(t, "DENIED_FIXTURE", "FIXTURE_ONLY")
			}
		})
	}

	index := get(t, base, "/")
	if index.status != 200 {
		t.Fatalf("GET / returned %d: %s", index.status, index.body)
	}
	index.contains(t, srv, "NESTED_APP_INDEX")
	index.excludes(t, "WORKSPACE_INDEX")

	// ── 2: the running server really serves bazel-bin ─────────────────────────
	// Vite answers 403 for a file outside fs.allow, so a 200 here is the whole
	// assertion: without the generated allow-list this request is denied.

	t.Run("serves_bazel_bin", func(t *testing.T) {
		r := get(t, base, "/@fs"+appBin+"/app.js")
		if r.status != 200 {
			t.Errorf("serving from bazel-bin returned %d, want 200 (server.fs.allow lost bazel-bin)\nbody:\n%s",
				r.status, r.body)
		}
	})

	// ── 3: where an import of a .ts module lands ──────────────────────────────
	// Vite rewrites every import specifier in what it serves to the URL it
	// resolved, so the served entry.js says which file the plugin container
	// chose for "./app.ts". In dev that must be the SOURCE in both variants:
	// Bazel is out of the inner loop, and the pre-compiled .js next to it is
	// exactly the round trip this is meant to skip.
	t.Run("serves_first_party_source", func(t *testing.T) {
		r := get(t, base, "/entry.js")
		if r.status != 200 {
			t.Fatalf("GET /entry.js returned %d, want 200\n%s", r.status, r.body)
		}
		t.Logf("entry.js = %s", r.body)
		moduleURL := "/app.ts"
		if impl == "oj" && wantBazelPlugin {
			moduleURL = depURL(r.body)
			if moduleURL == "" {
				t.Fatalf("entry.js has no resolved import URL: %s", r.body)
			}
		} else {
			r.contains(t, srv, "/app.ts")
		}
		r.excludes(t, "bazel-bin/app.js")

		m := get(t, base, moduleURL)
		if m.status != 200 {
			t.Fatalf("GET %s returned %d, want 200: %s", moduleURL, m.status, m.body)
		}
		m.contains(t, srv, "TS_SOURCE_TRANSFORMED_BY_VITE")
		m.excludes(t, "JS_PRECOMPILED_BY_BAZEL")
	})

	t.Run("serves_sibling_package", func(t *testing.T) {
		r := get(t, base, "/sibling_entry.js")
		if r.status != 200 {
			t.Fatalf("sibling importer returned %d: %s", r.status, r.body)
		}
		url := depURL(r.body)
		if url == "" {
			t.Fatalf("no sibling import URL in %s", r.body)
		}
		module := get(t, base, url)
		if module.status != 200 {
			t.Fatalf("sibling URL %s returned %d", url, module.status)
		}
		module.contains(t, srv, "SIBLING_SOURCE")
	})

	// ── 3b: what bazel-bin is still for ───────────────────────────────────────
	// A ts_codegen output has no checked-in source, so Vite cannot produce it and
	// the plugin has to find it under bazel-bin. Without the plugin the same
	// import resolves nowhere -- the one request that separates the two variants.
	t.Run("resolves_codegen_from_bazel_bin", func(t *testing.T) {
		r := get(t, base, "/gen_entry.js")
		t.Logf("gen_entry.js (status %d) = %s", r.status, r.body)
		if !wantBazelPlugin {
			if impl == "oj" {
				r.contains(t, srv, "./generated/routes.ts")
				r.excludes(t, "bazel-bin/generated/routes.ts")
				return
			}
			// Vite cannot see bazel-bin without the plugin, and fails the transform.
			if r.status == 200 {
				t.Fatalf("GET /gen_entry.js returned 200 without the plugin; bazel-bin "+
					"is not Vite's to resolve\n%s", r.body)
			}
			r.contains(t, srv, "Failed to resolve import")
			r.excludes(t, "bazel-bin/generated/routes.ts")
			return
		}
		if r.status != 200 {
			t.Fatalf("GET /gen_entry.js returned %d, want 200\n%s", r.status, r.body)
		}
		moduleURL := "/@fs" + filepath.Join(appBin, "generated", "routes.ts")
		if impl == "oj" {
			moduleURL = depURL(r.body)
			if moduleURL == "" {
				t.Fatalf("gen_entry.js has no resolved import URL: %s", r.body)
			}
		} else {
			r.contains(t, srv, moduleURL)
		}
		m := get(t, base, moduleURL)
		if m.status != 200 {
			t.Fatalf("GET %s returned %d, want 200: %s", moduleURL, m.status, m.body)
		}
		m.contains(t, srv, "CODEGEN_V1")
	})

	// ── 4b: a bare npm specifier ──────────────────────────────────────────────
	// The npm tree is a Bazel output; what puts it on the walk up from a
	// checked-in source file is the <workspace>/node_modules link the launcher
	// makes. The response says which file the server chose, and that file has to
	// be served too -- resolving into a directory server.fs.allow does not reach
	// would only move the failure one request later.
	t.Run("resolves_npm_from_bazel_tree", func(t *testing.T) {
		r := get(t, base, "/npm_entry.js")
		if r.status != 200 {
			t.Fatalf("GET /npm_entry.js returned %d, want 200\n%s\n%s", r.status, r.body, srv.log(t))
		}
		t.Logf("npm_entry.js = %s", r.body)
		dep := depURL(r.body)
		if dep == "" {
			t.Fatalf("nothing in the response points at a resolved dependency:\n%s", r.body)
		}
		m := get(t, base, dep)
		// Vite may serve the dependency directly or from its pre-bundle cache.
		if !strings.Contains(m.finalURL, "/node_modules/zod/") &&
			!strings.Contains(m.finalURL, "/vite-cache/deps/") {
			t.Errorf("`import \"zod\"` resolved to %q, which is neither a Bazel npm "+
				"tree nor this target's dependency cache", m.finalURL)
		}
		if m.status != 200 {
			t.Errorf("the resolved dependency %s answers %d, want 200\n%s", dep, m.status, m.body)
		}

		// Missing imports may fail during transformation or on the later module request.
		missing := get(t, base, "/npm_missing.js")
		if missing.status != 200 {
			missing.contains(t, srv, "Failed to resolve import")
			return
		}
		deferred := depURL(missing.body)
		if deferred == "" {
			t.Fatalf("GET /npm_missing.js returned 200 and resolved `not-in-any-npm-tree` "+
				"to nothing deferred either; the specifier was invented\n%s", missing.body)
		}
		if answer := get(t, base, deferred); answer.status == 200 {
			t.Errorf("the deferred reference for `not-in-any-npm-tree` answers 200 from %q; "+
				"a package no tree has must not resolve\n%s", answer.finalURL, answer.body)
		}
	})

	// ── 4c: the user-supplied vite_config ─────────────────────────────────────
	// The marker reaches the response only if the config loaded, its own bare npm
	// import resolved, and its plugin is in the container.
	t.Run("user_config_plugin", func(t *testing.T) {
		r := get(t, base, "/app.ts")
		if r.status != 200 {
			t.Fatalf("GET /app.ts returned %d, want 200\n%s", r.status, r.body)
		}
		if !wantUserConfig {
			r.excludes(t, "USER_CONFIG_PLUGIN_RAN")
			return
		}
		r.contains(t, srv, "USER_CONFIG_PLUGIN_RAN")
	})

	// ── 5: React Fast Refresh ─────────────────────────────────────────────────
	t.Run("react_refresh", func(t *testing.T) {
		r := get(t, base, "/widget.tsx")
		if r.status != 200 {
			t.Fatalf("GET /widget.tsx returned %d, want 200\n%s\n%s", r.status, r.body, srv.log(t))
		}
		if impl == "oj" {
			r.contains(t, srv, "$RefreshReg$")
			return
		}
		if !wantReactRefresh {
			r.excludes(t, "react-refresh", "$RefreshReg$")
			return
		}
		// The plugin has to have loaded AND run: the preamble is what preserves
		// component state, and plugin-react only emits it after resolving the
		// react-refresh runtime out of the same npm tree.
		r.contains(t, srv, "/@react-refresh", "$RefreshReg$")
	})

	// ── 6: the SIGTERM ibazel sends on every rebuild ──────────────────────────
	// ibazel terminates the launcher and rebuilds; the point of the dev server is
	// that vite lives through it and picks the new .js up from its watcher.
	t.Run("survives_ibazel_sigterm", func(t *testing.T) {
		if err := srv.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("SIGTERM to the launcher: %v", err)
		}
		for i := 0; i < 10; i++ {
			if done, err := srv.exited(); done {
				t.Fatalf("the launcher died on SIGTERM (%v); ibazel would take the dev server "+
					"down on every rebuild\n%s", err, srv.log(t))
			}
			time.Sleep(100 * time.Millisecond)
		}
		if r := get(t, base, "/app.ts"); r.status != 200 {
			t.Errorf("after SIGTERM the server answers %d, want 200\n%s", r.status, srv.log(t))
		}
	})

	if wantBazelPlugin {
		t.Run("generated_hmr_uses_served_url", func(t *testing.T) {
			file := filepath.Join(appBin, "hot.js")
			prefix := "GENERATED"
			if impl == "oj" {
				prefix = "DISK_ONLY"
			}
			write(t, file, `export const value = "`+prefix+`_V1"; if (import.meta.hot) import.meta.hot.accept();`)
			write(t, filepath.Join(appRoot, "hot_entry.js"), `import { value } from "./hot.js"; export { value };`)
			url := depURL(get(t, base, "/hot_entry.js").body)
			if url == "" {
				t.Fatal("generated module has no import URL")
			}
			initial := get(t, base, url)
			if initial.status != 200 {
				t.Fatalf("generated module returned %d: %s", initial.status, initial.body)
			}
			initial.contains(t, srv, "GENERATED_V1")
			url = strings.TrimPrefix(initial.finalURL, base)
			if impl == "oj" {
				initial.excludes(t, "DISK_ONLY")
				initial.contains(t, srv, "TRANSFORM_ONE")
				initial.contains(t, srv, "TRANSFORM_TWO")
				initial.contains(t, srv, "APPLICATION_GRAPH_URL=/app.ts")
				initial.contains(t, srv, "__oj_createHotContext("+strconv.Quote(url)+")")
				_, dataURL, hasMap := strings.Cut(initial.body, "sourceMappingURL=")
				media, encoded, hasData := strings.Cut(dataURL, ",")
				isJSON := media == "data:application/json;base64" || media == "data:application/json;charset=utf-8;base64"
				if !hasMap || !hasData || !isJSON || len(strings.Fields(encoded)) == 0 {
					t.Fatal("generated module has no inline source map")
				}
				mapped, err := base64.StdEncoding.DecodeString(strings.Fields(encoded)[0])
				if err != nil {
					t.Fatal(err)
				}
				var sourceMap struct {
					Sources        []string `json:"sources"`
					SourcesContent []string `json:"sourcesContent"`
					Mappings       string   `json:"mappings"`
				}
				if err := json.Unmarshal(mapped, &sourceMap); err != nil {
					t.Fatal(err)
				}
				if sourceMap.Mappings == "" || !strings.Contains(strings.Join(sourceMap.Sources, "\n"), "original-hot.ts") || !strings.Contains(strings.Join(sourceMap.SourcesContent, "\n"), "ORIGINAL_HOT") {
					t.Fatalf("generated source map lost the original source: %s", mapped)
				}
			}
			wsPath, protocol := "/", "vite-hmr"
			if impl == "oj" {
				wsPath, protocol = "/__ws", ""
			}
			sock, err := hmrsocket.Dial(strings.TrimPrefix(base, "http://"), wsPath, protocol)
			if err != nil {
				t.Fatal(err)
			}
			defer sock.Close()
			write(t, file, `export const value = "`+prefix+`_V2"; if (import.meta.hot) import.meta.hot.accept();`)
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				frame, err := sock.Next(time.Until(deadline))
				if err != nil {
					t.Fatalf("%v\n%s", err, srv.log(t))
				}
				var message struct {
					Updates []struct {
						Path string `json:"path"`
					} `json:"updates"`
				}
				if err := json.Unmarshal([]byte(frame), &message); err != nil {
					t.Fatal(err)
				}
				matched := false
				for _, update := range message.Updates {
					if update.Path != url && !strings.HasSuffix(update.Path, "/hot.js") {
						continue
					}
					if update.Path != url {
						t.Fatalf("generated module HMR URL = %q, want %q", update.Path, url)
					}
					response := get(t, base, update.Path)
					if response.status != 200 {
						t.Fatalf("HMR URL %s returned %d", update.Path, response.status)
					}
					response.contains(t, srv, "GENERATED_V2")
					matched = true
				}
				if matched {
					return
				}
			}
			t.Fatal("no generated module HMR URL was received")
		})
	}

	// ── 7: restart-or-keep, both ways ─────────────────────────────────────────
	// Only the plugin makes the decision; without it there is no ConfigWatcher
	// and nothing to assert.
	if !wantBazelPlugin {
		return
	}

	// A rebuild that only rewrote ts_codegen output leaves the running server
	// correctly configured, so it must serve the new bytes without restarting.
	t.Run("keeps_running_on_codegen_rebuild", func(t *testing.T) {
		before := restartCount(t, srv)
		write(t, generated, "export const routes: string[] = [\"CODEGEN_V2\"];\n")
		url := "/@fs" + filepath.Join(appBin, "generated", "routes.ts")
		if !eventually(t, func() bool {
			return strings.Contains(bodyOf(base, url), "CODEGEN_V2")
		}) {
			t.Errorf("the rebuilt codegen output never reached the server\n%s", srv.log(t))
		}
		if after := restartCount(t, srv); after != before {
			t.Errorf("a codegen-only rebuild restarted Vite (%d → %d restarts)\n%s",
				before, after, srv.log(t))
		}
	})

	// A rebuild that changed the generated config left the running server
	// configured for a graph that no longer exists, and only a restart fixes it.
	t.Run("restarts_on_config_change", func(t *testing.T) {
		before := restartCount(t, srv)
		write(t, scratchConfig, "// stand-in for the generated config, v2 — new aliases\n")
		if !eventually(t, func() bool { return restartCount(t, srv) > before }) {
			t.Fatalf("the config changed and Vite did not restart\n%s", srv.log(t))
		}
		if !eventually(t, func() bool { return answers(base, "/app.ts") }) {
			t.Errorf("the server never came back after restarting\n%s", srv.log(t))
		}
	})
}

func importURL(t *testing.T, body, filename, query string) string {
	t.Helper()
	for _, match := range regexp.MustCompile(`["']([^"']+)["']`).FindAllStringSubmatch(body, -1) {
		parsed, err := url.Parse(match[1])
		if err == nil && strings.HasPrefix(parsed.Path, "/") && filepath.Base(parsed.Path) == filename && (query == "" || parsed.Query().Has(query)) {
			return match[1]
		}
	}
	t.Fatalf("no %s URL with query %q in %s", filename, query, body)
	return ""
}

func restartCount(t *testing.T, s *server) int {
	t.Helper()
	log := s.log(t)
	return strings.Count(log, "[vite-plugin-bazel] restarting:") +
		strings.Count(log, "oj config/env changed — restarting dev server")
}

type devConfig struct {
	Port         int           `json:"port"`
	Host         any           `json:"host"`
	Root         string        `json:"root"`
	FsAllow      []string      `json:"fsAllow"`
	Alias        []aliasEntry  `json:"alias"`
	ConfigInputs []configInput `json:"configInputs"`
	Plugins      []string      `json:"plugins"`
	// resolve.modules is webpack's, not Vite's. Read back so a test can fail if
	// it returns.
	ResolveModules any `json:"resolveModules"`
	raw            string
}

type aliasEntry struct {
	Find        string `json:"find"`
	Replacement string `json:"replacement"`
}

type configInput struct {
	Label  string `json:"label"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Remedy string `json:"remedy"`
}

// replacementFor returns what the alias with this exact `find` points at, or ""
// when the config declared no such alias.
func replacementFor(entries []aliasEntry, find string) string {
	for _, entry := range entries {
		if entry.Find == find {
			return entry.Replacement
		}
	}
	return ""
}

// evalConfig runs the generated config as the module it is: the port, root,
// allow-list and plugin list it reports are all read out of its environment at
// import time, so it gets the environment the launcher would have given it.
func evalConfig(t *testing.T, node, readConfig, config string, env []string) devConfig {
	t.Helper()
	cmd := exec.Command(node, readConfig, config)
	cmd.Env = append(os.Environ(), env...)
	// stderr separately: the JSON is on stdout, and the reason it is not there is
	// on stderr.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("evaluating %s: %v\n%s%s", config, err, out, stderr.String())
	}
	cfg := devConfig{raw: string(out)}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("read_config.mjs printed %q, which is not JSON: %v", out, err)
	}
	return cfg
}
