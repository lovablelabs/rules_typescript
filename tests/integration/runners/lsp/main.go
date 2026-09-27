package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mikn/rules_typescript/tests/integration/harness"
	configreader "github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

type tsconfig struct {
	CompilerOptions struct {
		Paths    *configreader.Paths `json:"paths"`
		RootDir  string              `json:"rootDir"`
		RootDirs []string            `json:"rootDirs"`
	} `json:"compilerOptions"`
	RootDirSources []string `json:"rootDirSources"`
	Files          []string `json:"files"`
	Exclude        []string `json:"exclude"`
}

func readTsconfig(it *harness.IT, path string) tsconfig {
	it.RequireFile(path, "%s was not generated", path)
	parsed := tsconfig{}
	if err := json.Unmarshal([]byte(it.Read(path)), &parsed); err != nil {
		it.Fail("%s is not valid JSON: %v", path, err)
	}
	return parsed
}

func configPath(project, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(filepath.Dir(project), value)
}

func fileIdentity(it *harness.IT, file string) string {
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		it.Fail("cannot resolve file identity %s: %v", file, err)
	}
	return resolved
}

func configFileIdentities(it *harness.IT, project string, files []string) map[string]bool {
	identities := map[string]bool{}
	for _, file := range files {
		identities[fileIdentity(it, configPath(project, file))] = true
	}
	return identities
}

func assertEditorOutputsDoNotRemainBehindWorkspaceSymlinks(it *harness.IT, target string) {
	project := it.Path("generated/.bazel/tsconfig/" + target + ".json")
	build := it.Bin("generated/" + target + ".ide.tsconfig.json")
	before := readTsconfig(it, build)
	after := readTsconfig(it, project)
	canonical, err := filepath.EvalSymlinks(it.BazelBin())
	if err != nil {
		it.Fail("canonical output root is unavailable: %v", err)
	}
	const suffix = "generated/generated/types/*"
	want := filepath.ToSlash(filepath.Join(canonical, suffix))
	if !slices.Equal(after.CompilerOptions.Paths.Get("#types/*"), []string{want}) {
		it.Fail("editor tree alias still depends on the workspace output symlink: %v, want %s", after.CompilerOptions.Paths.Get("#types/*"), want)
	}
	values := before.CompilerOptions.Paths.Get("#types/*")
	if len(values) != 1 || !strings.HasSuffix(values[0], suffix) {
		it.Fail("compiler tree alias lost its output-root identity: %v", values)
	}
	prefix := strings.TrimSuffix(values[0], suffix)
	bound := strings.ReplaceAll(it.Read(build), prefix, filepath.ToSlash(canonical)+"/")
	wantRoot := it.WorkspaceDir
	if before.RootDirSources == nil {
		it.Fail("compiler omitted rootDir containment facts for %s", target)
	}
	for _, source := range before.RootDirSources {
		if !strings.HasPrefix(source, prefix) {
			it.Fail("compiler containment source %q is outside the declared output root %q", source, prefix)
		}
		canonicalSource := filepath.Join(canonical, strings.TrimPrefix(source, prefix))
		for {
			rel, err := filepath.Rel(wantRoot, canonicalSource)
			if err != nil {
				it.Fail("cannot bind containment source %s: %v", source, err)
			}
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				break
			}
			wantRoot = filepath.Dir(wantRoot)
		}
	}
	if configPath(project, before.CompilerOptions.RootDir) != it.WorkspaceDir || configPath(project, after.CompilerOptions.RootDir) != wantRoot {
		it.Fail("editor rootDir = %q, want compiler-required root %q; compiler rootDir = %q", after.CompilerOptions.RootDir, wantRoot, before.CompilerOptions.RootDir)
	}
	var expected map[string]any
	if err := json.Unmarshal([]byte(bound), &expected); err != nil {
		it.Fail("invalid projected editor artifact: %v", err)
	}
	var ordered tsconfig
	if err := json.Unmarshal([]byte(bound), &ordered); err != nil {
		it.Fail("invalid projected editor paths: %v", err)
	}
	expected["compilerOptions"].(map[string]any)["paths"] = ordered.CompilerOptions.Paths
	delete(expected, "rootDirSources")
	if wantRoot != it.WorkspaceDir {
		expected["compilerOptions"].(map[string]any)["rootDir"] = filepath.ToSlash(wantRoot)
	}
	encoded, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		it.Fail("cannot encode bound editor project: %v", err)
	}
	bound = string(encoded) + "\n"
	if it.Read(project) != bound {
		it.Fail("binding the editor output root changed authored fields or alias order, or left an output path behind the workspace symlink")
	}
}

func assertGeneratedSchemaProducer(it *harness.IT) {
	var graph struct {
		Targets []struct {
			ID    json.Number
			Label string
		}
		Actions []struct{ TargetID json.Number }
	}
	query := it.BazelStdout("aquery", `outputs(".*/generated/generated/schema[.]json", deps(//generated:source_ts))`, "--output=jsonproto")
	if err := json.Unmarshal([]byte(query), &graph); err != nil {
		it.Fail("decode generated JSON producers: %v", err)
	}
	if len(graph.Actions) != 1 {
		it.Fail("generated JSON has %d producing actions, want only //generated:schema", len(graph.Actions))
	}
	for _, target := range graph.Targets {
		if target.ID == graph.Actions[0].TargetID && target.Label == "//generated:schema" {
			return
		}
	}
	it.Fail("generated JSON is not produced solely by //generated:schema: %s", query)
}

func assertGeneratedNpmResolution(it *harness.IT, trace string) {
	generated, err := filepath.EvalSymlinks(it.Bin("generated/generated/types/first.d.ts"))
	if err != nil {
		it.Fail("canonical generated declaration is unavailable: %v", err)
	}
	npm, err := filepath.EvalSymlinks(it.Bin("node_modules/zod"))
	if err != nil {
		it.Fail("output-side zod package is unavailable: %v", err)
	}
	bin, err := filepath.EvalSymlinks(it.BazelBin())
	if err != nil || !strings.HasPrefix(npm, bin+string(filepath.Separator)) {
		it.Fail("zod package %q is outside the canonical output tree %q: %v", npm, bin, err)
	}
	frames := regexp.MustCompile(`(?ms)^======== Resolving module 'zod' from '(.*?)'\. ========\r?\n.*?^======== Module name 'zod' was ([^\r\n]+)`)
	for _, frame := range frames.FindAllStringSubmatch(trace, -1) {
		from, err := filepath.EvalSymlinks(frame[1])
		if err != nil || from != generated {
			continue
		}
		resolved, ok := strings.CutPrefix(frame[2], "successfully resolved to '")
		resolved, _, quoted := strings.Cut(resolved, "'")
		selected, err := filepath.EvalSymlinks(resolved)
		if !ok || !quoted || err != nil || !strings.HasPrefix(selected, npm+string(filepath.Separator)) || !strings.HasSuffix(selected, ".d.ts") {
			it.Fail("generated declaration resolved zod to %q, want a declaration inside %q: %s; %v", selected, npm, frame[2], err)
		}
		return
	}
	it.Fail("compiler did not report zod resolution from canonical generated declaration %s", generated)
}

func assertAuthoredJSONResolution(it *harness.IT, trace, specifier string) {
	selected := regexp.MustCompile(`(?m)^======== Module name '` + regexp.QuoteMeta(specifier) + `' was successfully resolved to '([^']+)'\.`).FindStringSubmatch(trace)
	if len(selected) != 2 || filepath.Clean(selected[1]) != it.Path("authored/settings.json") {
		it.Fail("editor did not resolve the relative JSON import to the authored file: %v\n%s", selected, trace)
	}
}

func assertConfiguredProducerCannotReplaceInstalledEditorProject(it *harness.IT, compiler, executionRoot string) {
	it.Write(it.Path("configured_editor/source.bzl"), `def _source_impl(ctx):
    source = ctx.actions.declare_file("generated/value.ts")
    ctx.actions.write(source, "export const value: string = null;\n")
    return [DefaultInfo(files = depset([source], order = "postorder"))]

source = rule(implementation = _source_impl)

def _config_impl(ctx):
    config = ctx.actions.declare_file("generated/" + ctx.label.name + ".json")
    contents = {"compilerOptions": {"strictNullChecks": False}}
    if ctx.file.base:
        contents = {"extends": "../" * len(config.dirname.split("/")) + ctx.file.base.path}
    ctx.actions.write(config, json.encode(contents))
    return [DefaultInfo(files = depset([config], order = "postorder"))]

config = rule(
    implementation = _config_impl,
    attrs = {"base": attr.label(allow_single_file = True)},
)

def _configuration_impl(settings, attr):
    mode = settings["//command_line_option:compilation_mode"]
    return {"//command_line_option:compilation_mode": "dbg" if mode == "opt" else "opt"}

_configuration = transition(
    implementation = _configuration_impl,
    inputs = ["//command_line_option:compilation_mode"],
    outputs = ["//command_line_option:compilation_mode"],
)

def _configured_input_impl(ctx):
    target = ctx.attr.target
    if type(target) == "list":
        target = target[0]
    return [DefaultInfo(files = target[DefaultInfo].files)]

configured_input = rule(
    implementation = _configured_input_impl,
    attrs = {
        "target": attr.label(cfg = _configuration, mandatory = True),
        "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
    },
)
`)
	const build = `load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config", "ts_refresh_tsconfig")
load(":source.bzl", "config", "configured_input", "source")
source(name = "source")
config(name = "base")
config(name = "leaf", base = ":base")
configured_input(name = "configured_source", target = ":source")
configured_input(name = "configured_leaf", target = ":leaf")
configured_input(name = "configured_base", target = ":base")
ts_config(name = "tsconfig", src = ":leaf", deps = [":base"])
ts_compile(name = "consumer", srcs = [":source"], tsconfig = ":tsconfig")
ts_refresh_tsconfig(name = "refresh", tsconfig = None, generated_sources = True, deps = [":consumer"])
`
	buildPath := it.Path("configured_editor/BUILD.bazel")
	it.Write(buildPath, build)
	if err := os.Remove(it.Path("bazel-out")); err != nil && !os.IsNotExist(err) {
		it.Fail("cannot remove the default output symlink for configured producer: %v", err)
	}
	const prefix = "--symlink_prefix=bazel-editor-refresh-"
	it.MustBazel("run", prefix, "--run_validations=false", "--output_groups=-_validation", "//configured_editor:refresh")
	project := it.Path("configured_editor/.bazel/tsconfig/consumer.json")
	previous := it.Read(project)
	installed := readTsconfig(it, project)
	canonical, err := filepath.EvalSymlinks(it.Bin("configured_editor/generated/value.ts"))
	if err != nil {
		it.Fail("same-configuration producer is unavailable: %v", err)
	}
	if !configFileIdentities(it, project, installed.Files)[canonical] {
		it.Fail("same-configuration editor did not bind its generated root: %v, want %s", installed.Files, canonical)
	}
	log, err := it.Exec("configured-producer-installed.log", nil, compiler, "-p", project, "--noEmit", "--pretty", "false")
	if err != nil {
		it.Fail("same-configuration editor could not read its generated leaf and base config: %v\n%s", err, log.Text)
	}
	consumerRoot, err := filepath.Rel(executionRoot, it.BazelBin())
	if err != nil {
		it.Fail("cannot identify consuming output root: %v", err)
	}
	for _, test := range []struct{ name, target, shortPath, build string }{
		{
			name:      "source",
			target:    "configured_source",
			shortPath: "configured_editor/generated/value.ts",
			build:     strings.Replace(build, `srcs = [":source"]`, `srcs = [":configured_source"]`, 1),
		},
		{
			name:      "config-leaf",
			target:    "configured_leaf",
			shortPath: "configured_editor/generated/leaf.json",
			build:     strings.Replace(build, `src = ":leaf", deps = [":base"]`, `src = ":configured_leaf", deps = [":configured_base"]`, 1),
		},
		{
			name:      "config-base",
			target:    "configured_base",
			shortPath: "configured_editor/generated/base.json",
			build:     strings.Replace(strings.Replace(build, `base = ":base"`, `base = ":configured_base"`, 1), `deps = [":base"]`, `deps = [":configured_base"]`, 1),
		},
	} {
		it.Write(buildPath, test.build)
		it.MustBazel("build", prefix, "//configured_editor:consumer", "--output_groups=+_validation,+tsconfig")
		var inputs []struct{ Path, Root, ShortPath string }
		provenance := it.BazelStdout("cquery", prefix, "//configured_editor:"+test.target, "--output=starlark", `--starlark:expr=json.encode([{"Path": f.path, "Root": f.root.path, "ShortPath": f.short_path} for f in target.files.to_list()])`)
		if err := json.Unmarshal([]byte(provenance), &inputs); err != nil || len(inputs) != 1 {
			it.Fail("configured %s producer has no single artifact identity: %s; %v", test.name, provenance, err)
		}
		input := inputs[0]
		if input.Root == filepath.ToSlash(consumerRoot) || input.ShortPath != test.shortPath {
			it.Fail("local transitioned %s producer did not retain its distinct output root: %+v; consumer root %s", test.name, input, consumerRoot)
		}
		// Generated inputs compile at their compiler paths inside the program root; the validated build above checked them.
		it.RequireNoFile(it.Path("bazel-out"), "configured producer fixture recreated the default output symlink")
		log, err := it.BazelLog("configured-producer-"+test.name+"-refresh.log", "run", prefix, "--run_validations=false", "--output_groups=-_validation", "//configured_editor:refresh")
		if err == nil {
			it.Fail("foreign %s output root replaced the installed editor project with an unreadable generated input", test.name)
		}
		for _, required := range []string{"configured_editor/.bazel/tsconfig/consumer.json", input.Path, input.Root, filepath.ToSlash(consumerRoot), "does not support generated input", "output root", "Did you mean", "authored editor project without generated_sources", "Ordinary builds and type checking remain supported"} {
			if !strings.Contains(log.Text, required) {
				it.Fail("configured %s producer rejection omits %q: %v\n%s", test.name, required, err, log.Text)
			}
		}
		if it.Read(project) != previous {
			it.Fail("configured %s producer rejection replaced the installed editor project", test.name)
		}
		it.RequireNoFile(it.Path("bazel-out"), "configured producer refresh recreated the default output symlink")
	}
	it.Pass("configured source, leaf, and base producers cannot publish an unreadable foreign-root editor project or block ordinary compilation")
}

func main() {
	started := time.Now()
	harness.Run(harness.Config{
		Name:         "lsp",
		WorkspaceRel: "tests/integration/lsp",
		Renames: map[string]string{
			"generated/BUILD.bazel.tpl": "generated/BUILD.bazel",
			"authored/BUILD.bazel.tpl":  "authored/BUILD.bazel",
			"ambient/BUILD.bazel.tpl":   "ambient/BUILD.bazel",
		},
	}, func(it *harness.IT) {
		deadline := "0"
		if timeout := os.Getenv("TEST_TIMEOUT"); timeout != "" {
			seconds, err := strconv.ParseInt(timeout, 10, 64)
			if err != nil || seconds <= 0 {
				it.Fail("invalid enclosing test timeout %q", timeout)
			}
			deadline = strconv.FormatInt(started.Add(time.Duration(seconds)*time.Second).UnixMilli(), 10)
		}
		program := it.Read(it.Path("src/tsconfig.json"))
		authoredSettings := it.Read(it.Path("authored/settings.json"))

		it.Install()
		it.Pass("pnpm install")

		it.MustBazel("run", "//:gazelle")
		it.Pass("normal Gazelle derives the generated program before editor refresh")
		generatedBuild := it.Path("generated/BUILD.bazel")
		// The alias changes the refresh dependency's label, but the installed
		// project must retain the producing target's package-local origin.
		rootBuild := it.Path("BUILD.bazel")
		it.Write(rootBuild, it.Read(rootBuild)+`
alias(
    name = "public_editor",
    actual = "//generated:generated",
    visibility = ["//visibility:public"],
)
`)
		it.Replace(generatedBuild, `        ":generated",
        ":generated_test",
        ":source_ts",`, `        ":generated_test",
        ":source_ts",
        "//:public_editor",`)
		convergedBuild := it.Read(generatedBuild)
		convergedAuthoredBuild := it.Read(it.Path("authored/BUILD.bazel"))
		convergedAncestorBuild := it.Read(it.Path("authored/nested/BUILD.bazel"))
		convergedAmbientBuild := it.Read(it.Path("ambient/BUILD.bazel"))
		if !slices.Contains(strings.Fields(it.BazelStdout("query", "labels(srcs, //authored:authored)")), "//authored:settings.json") {
			it.Fail("normal Gazelle did not put the sibling JSON in its producer's srcs")
		}
		for _, dependency := range []struct{ target, label string }{
			{"//authored/nested:nested", "//authored:authored"},
			{"//authored/nested:nested", "//generated:declarations"},
			{"//ambient:ambient", "//ambient:ambient_declarations"},
		} {
			if !slices.Contains(strings.Fields(it.BazelStdout("query", "labels(deps, "+dependency.target+")")), dependency.label) {
				it.Fail("normal Gazelle omitted %s dependency %s", dependency.target, dependency.label)
			}
		}
		assertGeneratedSchemaProducer(it)
		authoredSource := it.Read(it.Path("authored/value.ts"))
		authoredConfig := it.Read(it.Path("authored/tsconfig.json"))
		authoredManifest := it.Read(it.Path("authored/package.json"))
		it.MustBazel("build", "//authored:authored", "--output_groups=+declarations")
		it.RequireContains(it.Bin("authored/value.d.ts"), "authoredValue: string", "authored dependency did not emit declarations")
		it.MustBazel("build", "@rules_typescript//ts/toolchain:tsgo_resolved")
		compiler := strings.TrimSpace(it.BazelStdout("cquery", "@rules_typescript//ts/toolchain:tsgo_resolved", "--output=starlark", `--starlark:expr=providers(target)["DefaultInfo"].files_to_run.executable.path`))
		executionRoot := strings.TrimSpace(it.BazelStdout("info", "execution_root"))
		compiler = filepath.Join(executionRoot, compiler)
		it.MustBazel("build", "//authored/nested:nested", "//ambient:ambient", "--output_groups=+_validation,+tsconfig")
		it.RequireNoFile(it.Bin("generated/generated.ide.tsconfig.json"), "ordinary build requested the editor projection")
		ordinaryProject := it.Read(it.Bin("authored/nested/nested.tsconfig.json"))
		ambientProject := it.Read(it.Bin("ambient/ambient.tsconfig.json"))
		ambientConsumer := it.Path("ambient/src/ambient-consumer.ts")
		ambientSource := it.Read(ambientConsumer)
		// A declared source keeps its own File over a generated tree's file at the same compiler path.
		it.Write(ambientConsumer, strings.Replace(ambientSource, "string | number", "number", 1))
		control, err := it.BazelLog("ordinary-declaration-collision.log", "build", "//ambient:ambient")
		if err == nil || !strings.Contains(control.Text, "error TS2322:") || !strings.Contains(control.Text, "Type 'string' is not assignable to type 'number'") {
			it.Fail("ordinary build stopped selecting the declared authored declaration: %v\n%s", err, control.Text)
		}
		it.Write(ambientConsumer, ambientSource)
		it.MustBazel("build", "//ambient:ambient", "--output_groups=+_validation,+tsconfig")

		editorPath := it.Path("generated/.bazel/tsconfig/generated_test.json")
		emittedEditorPath := it.Path("generated/.bazel/tsconfig/generated.json")
		ancestorEditorPath := it.Path("authored/nested/.bazel/tsconfig/nested.json")
		ambientEditorPath := it.Path("ambient/.bazel/tsconfig/ambient.json")
		it.RequireNoFile(emittedEditorPath, "producer's editor config must start absent before aliased refresh")
		assertEditorInputs := func(current string) {
			for _, target := range []string{"generated", "generated_test"} {
				assertEditorOutputsDoNotRemainBehindWorkspaceSymlinks(it, target)
			}
			editor := readTsconfig(it, editorPath)
			if !configFileIdentities(it, editorPath, editor.Files)[fileIdentity(it, it.Path("generated/standalone.test.ts"))] {
				it.Fail("test editor lost its standalone test root")
			}
			targets := editor.CompilerOptions.Paths.Get("#schema")
			if len(targets) != 1 {
				it.Fail("generated JSON alias must select one canonical file, got %v", targets)
			}
			selected, err := filepath.EvalSymlinks(configPath(editorPath, targets[0]))
			if err != nil {
				it.Fail("generated JSON alias does not resolve to a materialized file: %v", err)
			}
			canonical, err := filepath.EvalSymlinks(it.Bin("generated/generated/schema.json"))
			if err != nil || selected != canonical {
				it.Fail("generated JSON alias selects %q, want canonical output %q: %v", selected, canonical, err)
			}
			var schema struct {
				Current string `json:"current"`
			}
			if err := json.Unmarshal([]byte(it.Read(selected)), &schema); err != nil || schema.Current != current {
				it.Fail("generated JSON has current = %q, want %q: %v", schema.Current, current, err)
			}
			if len(editor.CompilerOptions.RootDirs) != 0 {
				it.Fail("editor retained the build overlay for authored relative imports: %v", editor.CompilerOptions.RootDirs)
			}
			emitted := readTsconfig(it, emittedEditorPath)
			if configFileIdentities(it, emittedEditorPath, emitted.Files)[fileIdentity(it, it.Path("generated/generated/value.ts"))] {
				it.Fail("editor selected the emitted dependency's original generated source: %v", emitted.Files)
			}
			values := emitted.CompilerOptions.Paths.Get("#generated/value")
			if len(values) != 1 {
				it.Fail("emitted dependency alias must select one canonical declaration, got %v", values)
			}
			selected, err = filepath.EvalSymlinks(configPath(emittedEditorPath, values[0]))
			canonical, canonicalErr := filepath.EvalSymlinks(it.Bin("generated/generated/value.d.ts"))
			if err != nil || canonicalErr != nil || selected != canonical {
				it.Fail("emitted dependency alias selects %q, want canonical declaration %q: %v, %v", selected, canonical, err, canonicalErr)
			}
			if it.Read(it.Path("authored/value.ts")) != authoredSource || it.Read(it.Path("authored/tsconfig.json")) != authoredConfig || it.Read(it.Path("authored/package.json")) != authoredManifest {
				it.Fail("refresh changed the authored dependency")
			}
		}

		refreshGenerated := func() {
			it.MustBazel("run", "--symlink_prefix=bazel-editor-refresh-", "--run_validations=false", "--output_groups=-_validation", "//generated:refresh_generated")
		}
		ambientTwin := it.Path("ambient/src/generated/ambient.d.ts")
		const staleAmbient = "declare module 'generated-api' { export const api: { added: string; replacement: string }; }\n"
		it.Write(ambientTwin, staleAmbient)
		authoredDeclaration := it.Path("ambient/src/generated/authored.d.ts")
		declarationSource := it.Read(authoredDeclaration)
		assertAmbient := func(phase string, present bool, diagnostic string) {
			project := readTsconfig(it, ambientEditorPath)
			canonical := it.Bin("ambient/src/generated/ambient.d.ts")
			included := false
			for _, entry := range project.Files {
				file := configPath(ambientEditorPath, entry)
				if strings.HasSuffix(filepath.ToSlash(file), "/src/generated/ambient.d.ts") {
					selected, err := filepath.EvalSymlinks(file)
					want, wantErr := filepath.EvalSymlinks(canonical)
					if err != nil || wantErr != nil || selected != want {
						it.Fail("%s: ambient root %q is not the canonical output %q: %v, %v", phase, file, canonical, err, wantErr)
					}
					included = true
				}
			}
			if included != present {
				it.Fail("%s: refresh ambient membership = %v, want %v: %v", phase, included, present, project.Files)
			}
			if !configFileIdentities(it, ambientEditorPath, project.Files)[fileIdentity(it, authoredDeclaration)] {
				it.Fail("%s: editor lost the explicit authored declaration identity: %v", phase, project.Files)
			}
			log, err := it.Exec("editor-ambient-"+phase+".log", nil, compiler, "-p", ambientEditorPath, "--noEmit", "--listFiles", "--pretty", "false")
			if diagnostic == "" && err != nil || diagnostic != "" && (err == nil || !strings.Contains(log.Text, "error "+diagnostic+":")) {
				it.Fail("%s: ambient diagnostic want %q: %v\n%s", phase, diagnostic, err, log.Text)
			}
			listed := map[string]bool{}
			for _, line := range strings.Split(log.Text, "\n") {
				file := strings.TrimSpace(line)
				if !filepath.IsAbs(file) {
					file = it.Path(file)
				}
				if resolved, err := filepath.EvalSymlinks(file); err == nil {
					listed[resolved] = true
				}
			}
			if listed[fileIdentity(it, ambientTwin)] || !listed[fileIdentity(it, authoredDeclaration)] || listed[fileIdentity(it, it.Bin("ambient/src/generated/authored.d.ts"))] {
				it.Fail("%s: editor confused authored declarations and generated checkout twins:\n%s", phase, log.Text)
			}
			if it.Read(ambientTwin) != staleAmbient || it.Read(authoredDeclaration) != declarationSource {
				it.Fail("%s: refresh changed a checkout declaration", phase)
			}
			if it.Read(it.Bin("authored/nested/nested.tsconfig.json")) != ordinaryProject || it.Read(it.Bin("ambient/ambient.tsconfig.json")) != ambientProject {
				it.Fail("%s: editor refresh changed the ordinary build project", phase)
			}
		}
		buildConfig := it.Read(it.Path("generated/tsconfig.build.json"))
		solutionConfig := it.Read(it.Path("generated/tsconfig.json"))
		it.Write(it.Path("generated/generated/value.ts"), "export const generatedValue: number = 1;\ndeclare global { interface GeneratedGlobals { removed: string; } }\n")
		stale := it.Read(it.Path("generated/generated/value.ts"))
		it.Write(it.Path("generated/generated/types/removed.d.ts"), "export declare const removed: string;\n")
		jsonTwin := it.Path("generated/generated/schema.json")
		it.RequireNoFile(jsonTwin, "generated JSON checkout twin must start absent")
		if err := os.Remove(it.Path("bazel-out")); err != nil {
			it.Fail("cannot remove the default workspace output symlink: %v", err)
		}
		it.RequireNoFile(it.Bin("generated/package.json"), "ordinary dependencies unexpectedly materialized the generated program's package scope before clean refresh")
		refreshGenerated()
		it.RequireFile(it.Bin("generated/package.json"), "clean refresh did not materialize the owner's as-built package scope")
		generatedManifest := it.Read(it.Path("generated/package.json"))
		stagedManifest := it.Read(it.Bin("generated/package.json"))
		it.RequireNoFile(it.Path("bazel-out"), "editor refresh recreated the default workspace output symlink")
		it.RequireFile(emittedEditorPath, "aliased refresh did not install the config at its producer's package-local path")
		it.RequireFile(ancestorEditorPath, "refresh did not install the ancestor JSON consumer's project")
		it.RequireFile(ambientEditorPath, "refresh did not install the ambient generated-tree project")
		for _, name := range []string{"leaf.json", "base.json"} {
			it.RequireFile(it.Bin("generated/generated/"+name), "refresh did not materialize generated config %s", name)
		}
		it.RequireNoFile(it.Path(".bazel/tsconfig/public_editor.json"), "aliased refresh installed the config relative to the alias")
		assertEditorInputs("first,removed")
		it.Write(ambientConsumer, strings.Replace(ambientSource, "string | number", "string", 1))
		assertAmbient("initial", true, "")
		it.Write(it.Path("ambient/ambient.json"), "[\"replacement\"]\n")
		refreshGenerated()
		assertAmbient("replacement", true, "TS2339")
		it.Write(ambientConsumer, strings.Replace(it.Read(ambientConsumer), "api.added", "api.replacement", 1))
		assertAmbient("replacement-repair", true, "")
		it.Write(it.Path("ambient/ambient.json"), "[]\n")
		refreshGenerated()
		it.RequireNoFile(it.Bin("ambient/src/generated/ambient.d.ts"), "deleted ambient declaration remains in the output tree")
		assertAmbient("deleted", false, "TS2307")
		it.Write(it.Path("ambient/ambient.json"), "[\"added\"]\n")
		it.Write(ambientConsumer, strings.Replace(ambientSource, "string | number", "string", 1))
		refreshGenerated()
		assertAmbient("recovered", true, "")
		it.Write(ambientConsumer, ambientSource)
		it.Pass("real refresh retains ambient roots selected by src/**/*.ts, preserves authored declaration collisions, rejects deleted checkout twins and recovers")
		if it.Read(it.Path("authored/settings.json")) != authoredSettings {
			it.Fail("refresh changed the dependency's authored JSON")
		}
		it.RequireNoFile(jsonTwin, "refresh wrote a generated JSON checkout twin")
		for _, path := range []string{"generated/generated/value.ts", "generated/generated/schema.json", "generated/generated/types/first.d.ts", "generated/generated/types/removed.d.ts"} {
			it.RequireFile(filepath.Join(it.BazelBin(), path), "refresh did not materialize %s", path)
		}
		if it.Read(it.Path("generated/generated/value.ts")) != stale {
			it.Fail("refresh changed existing source generator output")
		}
		if it.Read(it.Path("generated/tsconfig.build.json")) != buildConfig || it.Read(it.Path("generated/tsconfig.json")) != solutionConfig {
			it.Fail("refresh changed authored configuration")
		}
		initialGenerated := it.Read(filepath.Join(it.BazelBin(), "generated/generated/value.ts"))
		it.Write(it.Path("generated/names.json"), "[\"first\"]\n")
		const staleJSON = "{\"stale\":true}\n"
		it.Write(jsonTwin, staleJSON)
		refreshGenerated()
		assertEditorInputs("first")
		if it.Read(jsonTwin) != staleJSON {
			it.Fail("refresh changed the stale generated JSON checkout twin")
		}
		it.RequireNoFile(filepath.Join(it.BazelBin(), "generated/generated/types/removed.d.ts"), "deleted generated tree child remains")
		if it.Read(filepath.Join(it.BazelBin(), "generated/generated/value.ts")) == initialGenerated {
			it.Fail("generator input edit did not refresh output")
		}
		it.RequireContains(it.Path("generated/generated/types/removed.d.ts"), "removed", "refresh changed the stale source tree")
		it.MustBazel("build", "//generated:generated", "//generated:generated_test")
		if it.Read(it.Path("generated/package.json")) != generatedManifest || it.Read(it.Bin("generated/package.json")) != stagedManifest {
			it.Fail("ordinary build changed authored or staged package scope bytes after clean refresh")
		}
		originalConsumer := it.Read(it.Path("generated/consumer.ts"))
		it.Write(it.Path("generated/consumer.ts"), originalConsumer+"\nexport const removed: GeneratedGlobals['removed'] = 'stale';\n")
		log, err := it.Exec("editor-removed-global.log", nil, compiler, "-p", emittedEditorPath, "--noEmit", "--listFiles", "--pretty", "false")
		if err == nil || !strings.Contains(log.Text, "error TS2339:") || !strings.Contains(log.Text, "removed") {
			it.Fail("stale generated source retained the removed global member: %v\n%s", err, log.Text)
		}
		listed := map[string]bool{}
		for _, line := range strings.Split(log.Text, "\n") {
			file := strings.TrimSpace(line)
			if filepath.IsAbs(file) {
				if resolved, err := filepath.EvalSymlinks(file); err == nil {
					listed[resolved] = true
				}
			}
		}
		for file, present := range map[string]bool{
			it.Path("generated/standalone.test.ts"):  false,
			it.Path("generated/generated/value.ts"):  false,
			it.Bin("generated/generated/value.d.ts"): true,
		} {
			resolved, err := filepath.EvalSymlinks(file)
			if err != nil || listed[resolved] != present {
				it.Fail("library editor membership for %s = %v, want %v: %v\n%s", file, listed[resolved], present, err, log.Text)
			}
		}
		it.Write(it.Path("generated/consumer.ts"), originalConsumer)
		log, err = it.Exec("editor-current-global.log", nil, compiler, "-p", emittedEditorPath, "--noEmit", "--traceResolution", "--pretty", "false")
		if err != nil {
			it.Fail("editor did not retain the current emitted global member: %v\n%s", err, log.Text)
		}
		assertGeneratedNpmResolution(it, log.Text)
		assertAuthoredJSONResolution(it, log.Text, "../authored/settings.json")
		jsonConsumers := []struct{ name, source, project, specifier, original string }{
			{"sibling", "generated/consumer.ts", emittedEditorPath, "../authored/settings.json", originalConsumer},
			{"ancestor", "authored/nested/consumer.ts", ancestorEditorPath, "../settings.json", it.Read(it.Path("authored/nested/consumer.ts"))},
		}
		log, err = it.Exec("editor-ancestor-json.log", nil, compiler, "-p", ancestorEditorPath, "--noEmit", "--traceResolution", "--pretty", "false")
		if err != nil {
			it.Fail("editor rejected authored ancestor JSON beside generated declarations: %v\n%s", err, log.Text)
		}
		for _, specifier := range []string{"#authored", "authored-fixture/value"} {
			selected := regexp.MustCompile(`(?m)^======== Module name '` + regexp.QuoteMeta(specifier) + `' was successfully resolved to '([^']+)'\.`).FindStringSubmatch(log.Text)
			if len(selected) != 2 || filepath.Clean(selected[1]) != it.Path("authored/value.ts") {
				it.Fail("editor did not resolve %s through the authored ancestor manifest: %v\n%s", specifier, selected, log.Text)
			}
		}
		assertAuthoredJSONResolution(it, log.Text, "../settings.json")
		sourceProject := it.Path("generated/.bazel/tsconfig/source_ts.json")
		sourceArtifact := readTsconfig(it, it.Bin("generated/source_ts.ide.tsconfig.json"))
		if len(sourceArtifact.RootDirSources) != 1 || !strings.HasSuffix(sourceArtifact.RootDirSources[0], "/generated/generated/value.ts") {
			it.Fail("generated TypeScript program root has no exact containment proof: %v", sourceArtifact.RootDirSources)
		}
		wantSourceRoot := filepath.VolumeName(it.WorkspaceDir) + string(filepath.Separator)
		if got := configPath(sourceProject, readTsconfig(it, sourceProject).CompilerOptions.RootDir); got != wantSourceRoot {
			it.Fail("generated TypeScript editor rootDir = %q, want filesystem volume root %q", got, wantSourceRoot)
		}
		log, err = it.Exec("editor-generated-config.log", nil, compiler, "-p", sourceProject, "--noEmit", "--pretty", "false")
		if err != nil {
			it.Fail("editor could not read the generated leaf and base config: %v\n%s", err, log.Text)
		}
		it.Pass("cross-package alias refresh installs a producer-relative config that resolves generated declarations and their npm imports")
		const editedSettings = "{\"value\":42}\n"
		it.Write(it.Path("authored/settings.json"), editedSettings)
		refreshGenerated()
		assertEditorInputs("first")
		if it.Read(it.Path("authored/settings.json")) != editedSettings {
			it.Fail("refresh replaced the edited authored JSON with a stale copy")
		}
		for _, consumer := range jsonConsumers {
			log, err = it.Exec("editor-authored-json-edit-"+consumer.name+".log", nil, compiler, "-p", consumer.project, "--noEmit", "--traceResolution", "--pretty", "false")
			if err == nil || !regexp.MustCompile(`consumer\.ts\([0-9]+,[0-9]+\): error TS2322: Type 'number' is not assignable to type 'string'`).MatchString(log.Text) {
				it.Fail("%s editor did not observe the authored JSON's changed value type: %v\n%s", consumer.name, err, log.Text)
			}
			assertAuthoredJSONResolution(it, log.Text, consumer.specifier)
			it.Write(it.Path(consumer.source), strings.Replace(consumer.original, "export const setting: string", "export const setting: number", 1))
		}
		refreshGenerated()
		for _, consumer := range jsonConsumers {
			log, err = it.Exec("editor-authored-json-repair-"+consumer.name+".log", nil, compiler, "-p", consumer.project, "--noEmit", "--traceResolution", "--pretty", "false")
			if err != nil {
				it.Fail("%s editor did not accept the repaired authored JSON consumer: %v\n%s", consumer.name, err, log.Text)
			}
			assertAuthoredJSONResolution(it, log.Text, consumer.specifier)
			it.Write(it.Path(consumer.source), consumer.original)
		}
		it.Write(it.Path("authored/settings.json"), authoredSettings)
		it.Pass("refresh preserves sibling and ancestor dependency JSON and both editors read edits from its source path")
		it.Write(it.Path("generated/consumer.ts"), originalConsumer+"\nexport const broken: number = 'editor repair remains possible';\n")
		refreshGenerated()
		if it.Read(it.Path("generated/tsconfig.build.json")) != buildConfig || it.Read(it.Path("generated/tsconfig.json")) != solutionConfig {
			it.Fail("refresh with semantic errors changed authored configuration")
		}
		it.Write(it.Path("generated/consumer.ts"), originalConsumer)
		refreshGenerated()
		it.Pass("refresh materializes changed generated inputs, removes deleted outputs and preserves authored files with semantic errors")
		it.Pass("ordinary refresh selects canonical JSON with absent and stale checkout twins and retains authored relative imports beside generated tree inputs")

		func() {
			originalBuild := it.Read(generatedBuild)
			defer it.Write(generatedBuild, originalBuild)
			preserved := map[string]string{}
			for _, path := range []string{it.Path("generated/tsconfig.json"), it.Path("generated/tsconfig.build.json"), editorPath, emittedEditorPath, ancestorEditorPath, ambientEditorPath, it.Path("generated/.bazel/tsconfig/source_ts.json")} {
				preserved[path] = it.Read(path)
			}
			it.Write(generatedBuild, originalBuild+`
ts_refresh_tsconfig(name = "refresh_invalid", generated_sources = True, deps = [":generated"])
`)
			log, err := it.BazelLog("editor-invalid-generated-config.log", "run", "//generated:refresh_invalid")
			if err == nil {
				it.Fail("generated_sources with the default tsconfig installed an editor project")
			}
			for _, required := range []string{"generated_sources = True requires tsconfig = None", "Did you mean to keep an authored tsconfig.json solution wrapper", ".bazel/tsconfig/<target>.json"} {
				if !strings.Contains(log.Text, required) {
					it.Fail("invalid generated refresh omits %q: %v\n%s", required, err, log.Text)
				}
			}
			for path, before := range preserved {
				if it.Read(path) != before {
					it.Fail("invalid generated refresh changed authored or installed project %s", path)
				}
			}
		}()
		it.Pass("invalid generated refresh preserves authored configs and installed editor projects")

		it.MustBazel("run", "//:gazelle")
		it.Pass("bazel run //:gazelle")

		it.RequireNotContains(generatedBuild, ".bazel/tsconfig/", "Gazelle included derived editor projects as source inputs")
		if after := it.Read(generatedBuild); after != convergedBuild {
			it.Fail("generated editor journey changed the graph normal Gazelle derived before refresh")
		}
		refreshGenerated()
		it.MustBazel("run", "//:gazelle")
		if after := it.Read(generatedBuild); after != convergedBuild {
			it.Fail("refresh followed by normal Gazelle changed the converged generated program BUILD")
		}
		if err := os.RemoveAll(it.Path("generated/.bazel")); err != nil {
			it.Fail("remove derived editor project: %v", err)
		}
		it.MustBazel("run", "//:gazelle")
		if after := it.Read(generatedBuild); after != convergedBuild {
			it.Fail("removing the derived editor project changed the authored program graph")
		}
		it.Pass("normal Gazelle preserves the converged input graph with and without the editor project")
		if it.Read(it.Path("authored/BUILD.bazel")) != convergedAuthoredBuild {
			it.Fail("normal Gazelle changed the emitted authored dependency graph")
		}
		if it.Read(it.Path("authored/nested/BUILD.bazel")) != convergedAncestorBuild {
			it.Fail("normal Gazelle changed the ancestor JSON consumer graph")
		}

		if it.Read(it.Path("ambient/BUILD.bazel")) != convergedAmbientBuild {
			it.Fail("normal Gazelle changed the ambient generated-tree graph")
		}

		owners := strings.Fields(it.BazelStdout("query", `kind("ts_(compile|test) rule", //generated:* + //authored/... + //ambient:*)`))
		slices.Sort(owners)
		if !slices.Equal(owners, []string{"//ambient:ambient", "//authored/nested:nested", "//authored:authored", "//generated:generated", "//generated:generated_test", "//generated:source_ts"}) {
			it.Fail("Gazelle changed generated fixture program owners: %v", owners)
		}
		it.RequireContains(it.Path("generated/BUILD.bazel"), `runner = "@rules_typescript//ts/runners:node_test"`, "Gazelle dropped the standalone test runner")
		it.RequireContains(it.Path("generated/BUILD.bazel"), "emit = True", "Gazelle dropped the Node runner's required emitted program")

		// src/tsconfig.json maps @/* to ./* and button.ts imports @/models/user
		// through it; one program, so the alias resolves inside //src.
		build := it.Path("src/BUILD.bazel")
		it.RequireFile(build, "Gazelle did not generate src/BUILD.bazel")
		it.RequireContains(build, `tsconfig = ":tsconfig"`,
			"//src does not name the tsconfig that sets the alias")
		it.RequireContains(build, `"@npm//:zod"`,
			"//src does not depend on zod, which user.ts imports")
		it.RequireNotContains(build, "path_alias",
			"//src restates the alias through an attribute the rule does not have")
		for _, dir := range []string{"src/components", "src/models"} {
			it.RequireNoFile(it.Path(dir, "BUILD.bazel"),
				"Gazelle made %s a package; src/tsconfig.json lists its files", dir)
		}
		it.Pass("//src names src/tsconfig.json, owns both directories and " +
			"depends on zod")

		// refresh_tsconfig reads the @npm BUILD.bazel out of the output base, and
		// //... is what forces the repo rule to write it.
		it.MustBazel("build", "//...")
		it.Pass("bazel build //...: the aliased import resolves inside the program")

		it.MustBazel("run", "//:refresh_tsconfig")
		it.Pass("bazel run //:refresh_tsconfig")

		_, err = it.ExecWithOutput("generated-editor-session.log", os.Stdout, nil,
			it.Runfile("_main/ts/toolchain/node_resolved/node"),
			it.Runfile("_main/tests/lsp/generated_editor_test.mjs"),
			it.Runfile("_main/tests/lsp/node_modules/typescript/lib/tsserver.js"),
			it.WorkspaceDir, it.BazelExecutable(), it.BazelBin(), deadline)
		if err != nil {
			it.Fail("generated editor session failed: %v", err)
		}
		it.Pass("one tsserver session observes generated member addition, deletion and recreation through completions, navigation, hover and real errors, then restores the fixture")

		root := readTsconfig(it, it.Path("tsconfig.json"))
		it.Pass("tsconfig.json generated")

		// An npm package resolves through the checkout's node_modules, as under
		// tsc; a key here would send the editor somewhere the build does not look.
		for key := range root.CompilerOptions.Paths.Entries() {
			if key == "zod" || strings.HasPrefix(key, "zod/") {
				it.Fail("tsconfig.json names the npm package zod in paths: %q", key)
			}
		}
		it.Pass("tsconfig.json has no paths key for the npm package zod")

		// The package's own tsconfig.json is the editor program for its files:
		// nothing is written over it, and the root program leaves them out.
		if it.Read(it.Path("src/tsconfig.json")) != program {
			it.Fail("refresh_tsconfig rewrote src/tsconfig.json, the program " +
				"//src checks under")
		}
		for _, file := range []string{
			"src/components/button.ts", "src/models/user.ts",
		} {
			if !slices.Contains(root.Exclude, file) {
				it.Fail("tsconfig.json does not exclude %s, which "+
					"src/tsconfig.json checks; exclude = %v", file, root.Exclude)
			}
		}
		for _, dir := range []string{"src/components", "src/models"} {
			it.RequireNoFile(it.Path(dir, "tsconfig.json"),
				"refresh_tsconfig wrote %s/tsconfig.json; src/tsconfig.json "+
					"is the program there", dir)
		}
		it.Pass("src/tsconfig.json is left as written and the root excludes " +
			"the files it checks")

		// ts_pnpm's contract is a pnpm that runs from the workspace root with no
		// pnpm on the host; Install above ran it, and `--version` needs no network.
		out := it.BazelStdout("run", "//:pnpm", "--", "--version")
		version := ""
		for _, line := range strings.Split(out, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				version = trimmed
			}
		}
		if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`).MatchString(version) {
			it.Fail("bazel run //:pnpm -- --version printed %q, want a version number", version)
		}
		it.Pass("bazel run //:pnpm -- --version reported %s", version)

		// ts_add_package wraps the same binary with `add ... --lockfile-only`;
		// running it for real would need the registry.
		if err := it.Bazel("build", "//:add_package"); err != nil {
			it.Fail("//:add_package does not build")
		}
		it.Pass("//:add_package builds")

		assertConfiguredProducerCannotReplaceInstalledEditorProject(it, compiler, executionRoot)

		external := it.Scratch("external_editor")
		it.Write(it.Path("MODULE.bazel"), it.Read(it.Path("MODULE.bazel"))+`
bazel_dep(name = "external_editor", version = "1.0")
local_path_override(module_name = "external_editor", path = `+strconv.Quote(external)+`)
`)
		it.Write(filepath.Join(external, "MODULE.bazel"), `module(name = "external_editor", version = "1.0")
bazel_dep(name = "rules_typescript", version = "0.0.0")
`)
		const ownerBuild = `load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_refresh_tsconfig")
package(default_visibility = ["//visibility:public"])
ts_compile(name = "types", srcs = ["value.ts"])
`
		it.Write(filepath.Join(external, "editor_owner/BUILD.bazel"), ownerBuild)
		it.Write(filepath.Join(external, "editor_owner/value.ts"), "export const externalValue: boolean = true;\n")
		localBuild := ownerBuild + `alias(name = "external_alias", actual = "@external_editor//editor_owner:types")
ts_refresh_tsconfig(name = "refresh_local", tsconfig = None, generated_sources = True, deps = [":types"])
`
		ownerBuildPath := it.Path("editor_owner/BUILD.bazel")
		it.Write(ownerBuildPath, localBuild)
		it.Write(it.Path("editor_owner/value.ts"), "export const localValue: string = 'local';\n")
		it.MustBazel("build", "@external_editor//editor_owner:types", "//editor_owner:external_alias", "//editor_owner:types")
		it.MustBazel("run", "//editor_owner:refresh_local")
		installedPath := it.Path("editor_owner/.bazel/tsconfig/types.json")
		installed := it.Read(installedPath)
		it.RequireContains(installedPath, "value.ts", "local baseline-only project has no source")
		for _, test := range []struct{ name, deps string }{
			{"external_only", `"@external_editor//editor_owner:types"`},
			{"colliding_owner", `":types", "@external_editor//editor_owner:types"`},
			{"aliased_colliding_owner", `":types", ":external_alias"`},
		} {
			it.Write(ownerBuildPath, localBuild+`
ts_refresh_tsconfig(name = "refresh_external", tsconfig = None, generated_sources = True, deps = [`+test.deps+`])
`)
			log, err := it.BazelLog("editor-owner-"+test.name+".log", "run", "//editor_owner:refresh_external")
			if err == nil {
				it.Fail("%s installed an external editor project", test.name)
			}
			for _, required := range []string{"Analysis of target", "failed", "external producing owner", "external_editor", "//editor_owner:types", "Did you mean", "without generated_sources", "Ordinary builds and type checking remain supported"} {
				if !strings.Contains(log.Text, required) {
					it.Fail("%s rejection omits %q: %v\n%s", test.name, required, err, log.Text)
				}
			}
			it.RequireNoFile(it.Bin("editor_owner/refresh_external.manifest.json"), "external editor rejection wrote an installation manifest")
			if it.Read(installedPath) != installed {
				it.Fail("%s rejection changed the installed local project", test.name)
			}
			it.RequireNoFile(it.Path("editor_owner/.bazel/tsconfig/external_alias.json"), "external editor alias installed a project under its local label")
		}
		it.Write(ownerBuildPath, localBuild)
		it.Pass("external baseline-only owners compile but refresh rejects their artifacts before installation, including colliding local owners and aliases")

		it.Write(filepath.Join(external, "editor_owner/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_codegen")
`+ownerBuild+`
ts_compile(name = "emitted_types", srcs = ["value.ts"], emit = True)
ts_binary(name = "generator", entry_point = "generate.mjs")
ts_codegen(name = "scalar", srcs = ["value.ts"], generator = ":generator", outs = ["generated/scalar.d.ts"], args = ["{out}"])
ts_codegen(name = "tree", srcs = ["value.ts"], generator = ":generator", out_dir = "generated/tree", args = ["{out}/value.d.ts"])
`)
		it.Write(filepath.Join(external, "editor_owner/generate.mjs"), `import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
mkdirSync(dirname(process.argv[2]), { recursive: true });
writeFileSync(process.argv[2], 'export declare const externalValue: boolean;\n');
`)
		externalRoot := strings.TrimSpace(it.BazelStdout("cquery", "@external_editor//editor_owner:types", "--output=starlark", "--starlark:expr=target.label.workspace_root"))
		if !strings.HasPrefix(externalRoot, "external/") {
			it.Fail("external dependency has no canonical repository path: %q", externalRoot)
		}
		binDir, err := filepath.Rel(executionRoot, it.BazelBin())
		if err != nil {
			it.Fail("relative output root for external dependency: %v", err)
		}
		for _, test := range []struct{ target, artifact, declaration string }{
			{"scalar", "generated/scalar.d.ts", "generated/scalar.d.ts"},
			{"tree", "generated/tree", "generated/tree/value.d.ts"},
			{"emitted_types", "value.d.ts", "value.d.ts"},
		} {
			provenance := it.BazelStdout("cquery", "@external_editor//editor_owner:"+test.target, "--output=starlark", `--starlark:expr=[f.short_path for name, info in providers(target).items() if name.endswith("%TsInfo") for owner in info.owners.to_list() for f in owner.generated_inputs.to_list()]`)
			identity := "../" + strings.TrimPrefix(externalRoot, "external/") + "/editor_owner/" + test.artifact
			if !strings.Contains(provenance, strconv.Quote(identity)) {
				it.Fail("external producer dropped generated File identity %s: %s", identity, provenance)
			}
			dependency := externalRoot + "/editor_owner/" + test.declaration
			config, err := json.Marshal(map[string]any{
				"compilerOptions": map[string]any{"strict": true, "module": "preserve", "moduleResolution": "bundler", "paths": map[string][]string{"#external": {"../" + filepath.ToSlash(filepath.Join(binDir, dependency))}}},
				"files":           []string{"consumer.ts"},
			})
			if err != nil {
				it.Fail("encode local external-dependency config: %v", err)
			}
			it.Write(it.Path("editor_dependency/tsconfig.build.json"), string(config))
			it.Write(it.Path("editor_dependency/consumer.ts"), "import { externalValue } from '#external';\nexport const value: boolean = externalValue;\n")
			it.Write(it.Path("editor_dependency/BUILD.bazel"), `load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_refresh_tsconfig")
ts_compile(name = "consumer", srcs = ["consumer.ts"], tsconfig = "tsconfig.build.json", deps = ["@external_editor//editor_owner:`+test.target+`"])
ts_refresh_tsconfig(name = "refresh", tsconfig = None, generated_sources = True, deps = [":consumer"])
`)
			it.MustBazel("build", "//editor_dependency:consumer", "--output_groups=+_validation,+tsconfig")
			log, err := it.Exec("external-dependency-"+test.target+".log", nil, compiler, "-p", it.Bin("editor_dependency/consumer.tsconfig.json"), "--noEmit", "--traceResolution", "--pretty", "false")
			if err != nil {
				it.Fail("ordinary compiler rejected %s external dependency: %v\n%s", test.target, err, log.Text)
			}
			selected := regexp.MustCompile(`(?m)^======== Module name '#external' was successfully resolved to '([^']+)'\.`).FindStringSubmatch(log.Text)
			if len(selected) != 2 {
				it.Fail("ordinary compiler did not select the external dependency: %s", log.Text)
			}
			actual, err := filepath.EvalSymlinks(selected[1])
			canonical, canonicalErr := filepath.EvalSymlinks(it.Bin(dependency))
			if err != nil || canonicalErr != nil || actual != canonical {
				it.Fail("external dependency resolved to %q, want canonical %q: %v, %v", actual, canonical, err, canonicalErr)
			}
			project := it.Path("editor_dependency/.bazel/tsconfig/consumer.json")
			const previous = "{\"files\":[],\"include\":[]}\n"
			it.Write(project, previous)
			log, err = it.BazelLog("external-dependency-refresh-"+test.target+".log", "run", "//editor_dependency:refresh")
			if err == nil {
				it.Fail("local editor project accepted %s external generated dependency", test.target)
			}
			for _, required := range []string{"editor_dependency/.bazel/tsconfig/consumer.json", "external generated input", "external_editor", "authored editor project", "without generated_sources", "ordinary builds and type checking remain supported"} {
				if !strings.Contains(log.Text, required) {
					it.Fail("external dependency rejection omits %q: %v\n%s", required, err, log.Text)
				}
			}
			if it.Read(project) != previous {
				it.Fail("external dependency rejection replaced the installed local project")
			}
		}
		it.Pass("local programs compile external generated scalar, tree and emitted declarations; generated refresh rejects them before replacing installed JSON")
	})
}
