// The tsgo step runs tsgo from a program root under the target's output
// directory: the action's sources at their paths, the chain's node_modules in.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// runTsgo lays the program root out, runs the command from it (checked against
// -check when given) and removes it: the node_modules walk needs a root.
func runTsgo(args []string) error {
	flags := flag.NewFlagSet("tsgo", flag.ExitOnError)
	root := flags.String("root", "", "the program root to lay out, under the target's output directory")
	absoluteCopyArgs := flags.Bool("absolute-copy-args", false, "pass copied file arguments as absolute paths")
	verifyCopies := flags.Bool("verify-copies", false, "fail if the tool changes a copied input")
	discoverConfig := flags.String("discover-tsconfig", "", "generated program config exposed to tools that discover tsconfig.json beside sources")
	var sources, copies, importers, inherited, overlays, manifests, toolEnv, runtimeScopes stringList
	var editor actionConfig
	flags.StringVar(&editor.out, "editor-config", "", "compiler project to project for the editor")
	flags.StringVar(&editor.baseline, "editor-baseline", "", "compiler baseline options")
	flags.StringVar(&editor.binDir, "editor-bin-dir", "", "compiler output tree")
	flags.Var(&editor.generatedDirectories, "editor-generated-directory", "workspace-relative generated directory (repeatable)")
	flags.Var(&editor.generatedFiles, "editor-generated-file", "workspace-relative generated file (repeatable)")
	editorOut := flags.String("editor-out", "", "compiler-resolved editor project output")
	flags.StringVar(&editor.editorPath, "editor-path", "", "editor project workspace destination")
	flags.Var(&sources, "source",
		"a declared compiler input linked at its logical source path (repeatable)")
	flags.Var(&toolEnv, "tool-env", "NAME=executable input path, made absolute before entering the program root (repeatable)")
	flags.Var(&copies, "copy", "a source input copied at its workspace path for realpath-based module resolution (repeatable)")
	flags.Var(&importers, "node_modules",
		"an importer's node_modules directory, nearest first (repeatable); "+
			"the last is the lockfile's root importer")
	flags.Var(&inherited, "inherit_node_modules",
		"a dependency's or source's importer node_modules directory outside the chain, "+
			"resolving its own links before the chain's (repeatable)")
	flags.Var(&overlays, "overlay",
		"a generated compiler input or dependency output directory, laid at "+
			"its path relative to bin; JavaScript and original source package scopes "+
			"are not replaced (repeatable)")
	flags.Var(&manifests, "manifest",
		"the as-built package.json of a direct dependency at or above this "+
			"package, laid at its package's package.json unless an original source scope is present (repeatable)")
	flags.Var(&runtimeScopes, "runtime_scope", "a package scope and its declared runtime targets as JSON (repeatable)")
	check := flags.String("check", "",
		"the ownership manifest the --explainFiles listing is checked against; "+
			"without it the run's output is relayed and nothing is parsed")
	project := flags.String("tsconfig", "",
		"the target's tsconfig.json; with -check, an own src the listing "+
			"lacks that its chain's exclude names fails the step")
	stamp := flags.String("stamp", "", "file to create when tsgo exits 0")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cmdline := flags.Args()
	if *root == "" || len(cmdline) == 0 {
		return errors.New("tsgo needs -root=DIR and a command after --")
	}
	if err := validateRuntimeScopes(runtimeScopes); err != nil {
		return err
	}
	var own *ownership
	var chain *tsconfig.Resolved
	if *check != "" {
		var err error
		if own, err = readOwnership(*check); err != nil {
			return err
		}
		if *project != "" {
			if chain, err = tsconfig.Resolve(*project); err != nil {
				return err
			}
		}
	}
	err := editor.layOutProgramRoot(*root, sources, importers, inherited, overlays, manifests)
	if err != nil {
		return err
	}
	defer os.RemoveAll(*root)

	// Node resolves config imports from the real file, not its source symlink.
	for _, file := range copies {
		if file == outputTree || strings.HasPrefix(file, outputTree+"/") {
			return fmt.Errorf("-copy=%s is under the linked output tree", file)
		}
		at := filepath.Join(*root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return err
		}
		if err := os.Remove(at); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := copyFile(file, at); err != nil {
			return err
		}
	}
	if *discoverConfig != "" {
		if err := discoverProgramConfig(*root, *discoverConfig, sources); err != nil {
			return err
		}
	}

	tool, err := filepath.Abs(cmdline[0])
	if err != nil {
		return err
	}
	cmdline = append([]string{tool}, cmdline[1:]...)
	projected := make(map[string]string, len(sources))
	for _, source := range sources {
		projected[source] = compilerPath(source)
	}
	for i := 1; i < len(cmdline); i++ {
		if logical, ok := projected[cmdline[i]]; ok {
			cmdline[i] = logical
		}
	}
	if *absoluteCopyArgs {
		paths := make(map[string]string, len(copies))
		for _, file := range copies {
			absolute, err := filepath.Abs(filepath.Join(*root, filepath.FromSlash(file)))
			if err != nil {
				return err
			}
			paths[file] = absolute
		}
		for i := 1; i < len(cmdline); i++ {
			if absolute, ok := paths[cmdline[i]]; ok {
				cmdline[i] = absolute
			}
		}
	}

	env := make([]string, 0, len(toolEnv))
	for _, binding := range toolEnv {
		name, path, ok := strings.Cut(binding, "=")
		if !ok || name == "" || path == "" {
			return fmt.Errorf("-tool-env needs NAME=executable, got %q", binding)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		env = append(env, name+"="+absolute)
	}
	if editor.out != "" {
		editor.project = *project
		err = editorRun(*root, cmdline, editor, *editorOut, env...)
	} else if own == nil {
		err = runToolIn(*root, os.Stdout, cmdline, env...)
	} else {
		err = checkedRun(*root, cmdline, own, chain, *project, env...)
	}
	if err != nil {
		return err
	}
	if *verifyCopies {
		for _, file := range copies {
			original, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			staged, err := os.ReadFile(filepath.Join(*root, filepath.FromSlash(file)))
			if err != nil {
				return err
			}
			if !bytes.Equal(original, staged) {
				return fmt.Errorf("validation changed input %s; configure the tool in check-only mode", file)
			}
		}
	}
	if *stamp == "" {
		return nil
	}
	return os.WriteFile(*stamp, nil, 0o644)
}

func discoverProgramConfig(root, configArtifact string, sources []string) error {
	dir := filepath.Dir(binRelative(configArtifact))
	configs := map[string]bool{filepath.Join(dir, "tsconfig.json"): true}
	for _, source := range sources {
		source = filepath.FromSlash(compilerPath(source))
		if filepath.Base(source) == "tsconfig.json" && (dir == "." || strings.HasPrefix(source, dir+string(filepath.Separator))) {
			configs[source] = true
		}
	}
	for config := range configs {
		at := filepath.Join(root, config)
		data, err := json.Marshal(struct {
			Extends         string          `json:"extends"`
			CompilerOptions map[string]bool `json:"compilerOptions"`
		}{
			Extends:         fileRelative(filepath.ToSlash(filepath.Dir(config)), filepath.ToSlash(configArtifact)),
			CompilerOptions: map[string]bool{"noEmit": true},
		})
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return err
		}
		if err := os.Remove(at); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(at, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// checkedRun keeps the listing off stdout: a failing tsgo relays its
// diagnostics, or all it printed when none parse; a passing one is checked.
func checkedRun(dir string, cmdline []string, own *ownership,
	chain *tsconfig.Resolved, project string, env ...string) error {
	var out bytes.Buffer
	runErr := runToolIn(dir, &out, cmdline, env...)
	listing, parseErr := explainfiles.Parse(out.String())
	if runErr != nil {
		if parseErr != nil || len(listing.Diagnostics) == 0 {
			os.Stdout.Write(out.Bytes())
		} else {
			fmt.Println(strings.Join(listing.Diagnostics, "\n"))
		}
		return runErr
	}
	if parseErr != nil {
		return parseErr
	}
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	canonicalExecroot, err := filepath.EvalSymlinks(execroot)
	if err != nil {
		return err
	}
	rootAbs := filepath.Join(execroot, dir)
	if chain != nil && chain.Exclude != nil {
		logicalOwn := make(map[string]bool, len(own.own))
		originals := make(map[string]string, len(own.own))
		for file := range own.own {
			logical := compilerPath(file)
			logicalOwn[logical], originals[logical] = true, file
		}
		logicalFiles := make([]string, len(listing.Files))
		for i, file := range listing.Files {
			logicalFiles[i] = compilerPath(throughRoot(rootAbs, execroot, canonicalExecroot, file))
		}
		logicalChain := *chain
		logicalChain.ExcludeDir = compilerPath(chain.ExcludeDir)
		hits := excludedSrcs(logicalOwn, logicalFiles, &logicalChain, execroot)
		for i := range hits {
			hits[i].file = originals[hits[i].file]
		}
		if len(hits) > 0 {
			return errors.New(own.reportExcluded(project, hits))
		}
	}
	var npmInputs []npmInput
edges:
	for i, e := range listing.Edges {
		e.From = throughRoot(rootAbs, execroot, canonicalExecroot, e.From)
		listing.Edges[i].From = e.From
		if !own.own[e.From] {
			continue
		}
		at := filepath.FromSlash(e.To)
		if !filepath.IsAbs(at) {
			at = filepath.Join(rootAbs, at)
		}
		for _, base := range []string{execroot, canonicalExecroot} {
			if rel, ok := below(base, at); ok {
				if _, held := own.files[npmPackageRoot(rel)]; held {
					listing.Edges[i].To = rel
					continue edges
				}
			}
		}
		to := throughRoot(rootAbs, execroot, canonicalExecroot, e.To)
		if filepath.IsAbs(to) {
			if npmInputs == nil {
				npmInputs, err = declaredNpmInputs(execroot, own.files)
				if err != nil {
					return err
				}
			}
			to, err = throughNpmInputs(npmInputs, to)
			if err != nil {
				return err
			}
		}
		listing.Edges[i].To = to
	}
	findings, err := own.check(listing)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		return &undeclaredDeps{own.report(findings)}
	}
	return nil
}

type npmInput struct {
	file, physical string
}

func declaredNpmInputs(execroot string, files map[string][]fileOwner) ([]npmInput, error) {
	inputs := make([]npmInput, 0)
	for input := range files {
		if !strings.Contains(input, "/.pnpm/") || npmPackageRoot(input) != input {
			continue
		}
		physical, err := filepath.EvalSymlinks(filepath.Join(execroot, filepath.FromSlash(input)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve npm input %s: %w", input, err)
		}
		inputs = append(inputs, npmInput{file: input, physical: physical})
	}
	return inputs, nil
}

func throughNpmInputs(inputs []npmInput, listed string) (string, error) {
	at := filepath.Clean(filepath.FromSlash(listed))
	packageRoot := npmPackageRoot(filepath.ToSlash(at))
	var matched string
	for _, input := range inputs {
		member, ok := below(input.physical, at)
		if !ok {
			if packageRoot == "" {
				continue
			}
			member = strings.TrimPrefix(filepath.ToSlash(at)[len(packageRoot):], "/")
		}
		physical, err := filepath.EvalSymlinks(filepath.Join(input.physical, filepath.FromSlash(member)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("resolve npm input %s: %w", path.Join(input.file, member), err)
		}
		if physical != at {
			continue
		}
		candidate := path.Join(input.file, member)
		if matched != "" && matched != candidate {
			return "", fmt.Errorf("%s matches multiple declared npm store inputs", listed)
		}
		matched = candidate
	}
	if matched == "" {
		return listed, nil
	}
	return matched, nil
}

// throughRoot is the exec-root path of a listed file the root links, or the
// store path of a file under a chain link, which tsgo lists at the link.
func throughRoot(rootAbs, execroot, canonicalExecroot, listed string) string {
	at := filepath.FromSlash(listed)
	if !filepath.IsAbs(at) {
		at = filepath.Join(rootAbs, at)
	}
	// Type-package resolution can realpath a source link before listing it.
	if _, staged := belowLinkedRoot(rootAbs, at); !staged {
		if rel, ok := belowLinkedRoot(execroot, at); ok {
			listed = rel
		}
	}
	if root := npmPackageRoot(filepath.ToSlash(at)); root != "" {
		parent, err := filepath.EvalSymlinks(filepath.Dir(root))
		if err != nil {
			return listed
		}
		packagePath := filepath.Join(parent, filepath.Base(root))
		// Declared npm links are relative; absolute sandbox tree links retain their File coordinate.
		if target, err := os.Readlink(packagePath); err == nil && !filepath.IsAbs(target) {
			packagePath = filepath.Join(parent, target)
		}
		member := strings.TrimPrefix(filepath.ToSlash(at)[len(root):], "/")
		target := filepath.Join(packagePath, filepath.FromSlash(member))
		if rel, ok := below(canonicalExecroot, target); ok {
			return rel
		}
		return filepath.ToSlash(target)
	}
	target, err := os.Readlink(at)
	if err != nil {
		return listed
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(at), target)
	}
	if rel, ok := belowLinkedRoot(execroot, target); ok {
		return rel
	}
	return listed
}

// Compiler paths can bypass a symlinked checkout while source links retain it.
func belowLinkedRoot(root, target string) (string, bool) {
	if relative, ok := below(root, target); ok {
		return relative, true
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	return below(canonical, target)
}

func npmPackageRoot(p string) string {
	const marker = "node_modules/"
	i := strings.LastIndex(p, marker)
	if i < 0 || (i > 0 && p[i-1] != '/') {
		return ""
	}
	start := i + len(marker)
	name, _, _ := strings.Cut(p[start:], "/")
	if name == "" {
		return ""
	}
	end := start + len(name)
	if strings.HasPrefix(name, "@") {
		if end == len(p) {
			return ""
		}
		member, _, _ := strings.Cut(p[end+1:], "/")
		if member == "" {
			return ""
		}
		end += 1 + len(member)
	}
	return p[:end]
}

// below is target relative to dir when target is under it.
func below(dir, target string) (string, bool) {
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func underNodeModules(p string) bool {
	return strings.HasPrefix(p, "node_modules/") ||
		strings.Contains(p, "/node_modules/")
}

// Bazel's output tree, the top-level entry every action output is under.
const outputTree = "bazel-out"

func compilerPath(file string) string {
	if strings.HasPrefix(file, outputTree+"/") {
		return binRelative(file)
	}
	return file
}

// Generated roots need the original package scope beside their logical compiler path.
func layOutProgramRoot(
	root string, sources, importers, inherited, overlays, manifests []string,
) error {
	return (&actionConfig{}).layOutProgramRoot(root, sources, importers, inherited, overlays, manifests)
}

func (a *actionConfig) layOutProgramRoot(
	root string, sources, importers, inherited, overlays, manifests []string,
) error {
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	a.srcs = sources
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	var editor *editorProjection
	if a.out != "" {
		editor = &editorProjection{actionConfig: a, root: rootAbs, workspace: execroot}
		raw, err := os.ReadFile(a.out)
		if err != nil {
			return err
		}
		var config tsconfigFile
		if err := json.Unmarshal(raw, &config); err != nil {
			return err
		}
		editor.compilerOptions = config.CompilerOptions
		for _, key := range []string{"outDir", "declarationDir"} {
			if value, ok := config.CompilerOptions[key].(string); ok && value != "" {
				editor.outputContext = true
			}
		}
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := linkAt(root, execroot, outputTree); err != nil {
		return err
	}
	sourceFiles := map[string]string{}
	for _, file := range sources {
		logical := compilerPath(file)
		if !fs.ValidPath(logical) || logical == "." || logical == outputTree || strings.HasPrefix(logical, outputTree+"/") {
			return fmt.Errorf("-source=%s has no compiler source coordinate outside %s", file, outputTree)
		}
		if previous, ok := sourceFiles[logical]; ok {
			if previous != file {
				return fmt.Errorf("compiler inputs %q and %q occupy %q; declare one source per compiler path", previous, file, logical)
			}
			continue
		}
		sourceFiles[logical] = file
		if editor != nil && filepath.Base(file) == "package.json" {
			if err := editor.checkPackageContext(file, file, file); err != nil {
				return err
			}
		}
		at := filepath.Join(root, filepath.FromSlash(logical))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(execroot, filepath.FromSlash(file)), at); err != nil {
			return err
		}
	}
	// Empty parents let the compiler probe scalar identities without staging
	// their bytes or guessing which extensions its resolver will try.
	for _, file := range a.generatedFiles {
		if editor != nil && filepath.Base(file) == "package.json" {
			source := filepath.Join(a.binDir, file)
			if err := editor.checkPackageContext(source, source, source); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(filepath.FromSlash(file))), 0o755); err != nil {
			return err
		}
	}
	for _, binDir := range inherited {
		dir := importerDir(binDir)
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
		link := filepath.Join(root, dir, "node_modules")
		if err := os.RemoveAll(link); err != nil {
			return err
		}
		if len(importers) > 1 {
			// The root importer stays reachable by the ancestor walk.
			if err := mergeImporters(link, execroot, append([]string{binDir}, importers[:len(importers)-1]...)); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(filepath.Join(execroot, binDir), link); err != nil {
			return err
		}
	}
	for i, binDir := range importers {
		at := "node_modules"
		if i < len(importers)-1 {
			dir := importerDir(binDir)
			if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
				return err
			}
			at = filepath.Join(dir, "node_modules")
		}
		link := filepath.Join(root, at)
		if err := os.RemoveAll(link); err != nil {
			return err
		}
		if i == 0 && len(importers) > 2 && skipsAncestors(importers) {
			if err := mergeImporters(link, execroot, importers[:len(importers)-1]); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(filepath.Join(execroot, binDir), link); err != nil {
			return err
		}
	}
	if editor != nil {
		for _, directory := range a.generatedDirectories {
			if err := editor.checkGeneratedPackageContexts(directory); err != nil {
				return err
			}
		}
	}
	for _, encoded := range overlays {
		var pair []string
		if err := json.Unmarshal([]byte(encoded), &pair); err != nil || len(pair) != 2 || pair[0] == "" || (pair[1] != "" && !fs.ValidPath(pair[1])) {
			return fmt.Errorf("invalid compiler overlay pair %q", encoded)
		}
		file, logical := pair[0], pair[1]
		from := filepath.Join(execroot, filepath.FromSlash(file))
		rel := filepath.FromSlash(logical)
		st, err := os.Stat(from)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := overlayPath(root, rootAbs, from, rel, st, sourceFiles, false, editor); err != nil {
			return err
		}
	}
	for _, file := range manifests {
		from := filepath.Join(execroot, filepath.FromSlash(file))
		st, err := os.Stat(from)
		if err != nil {
			return err
		}
		rel := filepath.Join(filepath.FromSlash(binRelative(path.Dir(file))), "package.json")
		if err := overlayPath(root, rootAbs, from, rel, st, sourceFiles, true, editor); err != nil {
			return err
		}
	}
	return nil
}

// binRelative is the path under bazel-out/<cfg>/bin/, "" for the bin directory.
func binRelative(binPath string) string {
	if strings.HasSuffix(binPath, "/bin") {
		return ""
	}
	if i := strings.Index(binPath, "/bin/"); i >= 0 {
		return binPath[i+len("/bin/"):]
	}
	return binPath
}

// skipsAncestors reports an intermediate chain importer outside the nearest
// importer's ancestors, which a node_modules walk from its sources never reaches.
func skipsAncestors(importers []string) bool {
	nearest := filepath.ToSlash(importerDir(importers[0]))
	for _, binDir := range importers[1 : len(importers)-1] {
		dir := filepath.ToSlash(importerDir(binDir))
		if dir != "" && nearest != dir && !strings.HasPrefix(nearest, dir+"/") {
			return true
		}
	}
	return false
}

// mergeImporters lays a node_modules directory whose entries follow the
// importers' order: the first importer linking a name wins.
func mergeImporters(at, execroot string, importers []string) error {
	for _, binDir := range importers {
		if err := mergeEntries(at, filepath.Join(execroot, binDir), true); err != nil {
			return err
		}
	}
	return nil
}

func mergeEntries(at, from string, scopes bool) error {
	entries, err := os.ReadDir(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(at, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		name, source := entry.Name(), filepath.Join(from, entry.Name())
		if scopes && strings.HasPrefix(name, "@") && entry.IsDir() {
			if info, err := os.Lstat(filepath.Join(at, name)); err == nil && !info.IsDir() {
				continue
			}
			if err := mergeEntries(filepath.Join(at, name), source, false); err != nil {
				return err
			}
			continue
		}
		if _, err := os.Lstat(filepath.Join(at, name)); err == nil {
			continue
		}
		if err := os.Symlink(source, filepath.Join(at, name)); err != nil {
			return err
		}
	}
	return nil
}

// importerDir is the importer's directory in the exec root, read off its
// node_modules' bin-dir path bazel-out/<cfg>/bin/<dir>/node_modules.
func importerDir(binDir string) string {
	dir := filepath.Dir(filepath.FromSlash(binRelative(binDir)))
	if dir == "." {
		return ""
	}
	return dir
}

// An explicit compiler root must keep the File named by its ownership record.
func overlayPath(root, rootAbs, from, rel string, st os.FileInfo, sourceFiles map[string]string, replaceManifest bool, editor *editorProjection) error {
	if editor != nil && editor.authored(rel) {
		return nil
	}
	if !st.IsDir() {
		if isJavaScript(filepath.Base(from)) || sourceFiles[filepath.ToSlash(rel)] != "" {
			return nil
		}
		if editor != nil && filepath.Base(rel) == "package.json" {
			if err := editor.checkPackageNamespace(from, rel); err != nil {
				return err
			}
		}
		at := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return err
		}
		if !replaceManifest && (strings.HasSuffix(rel, ".d.ts") || strings.HasSuffix(rel, ".d.mts") || strings.HasSuffix(rel, ".d.cts") || strings.HasSuffix(rel, ".json")) {
			if _, err := os.Lstat(at); err == nil {
				previous, err := filepath.EvalSymlinks(at)
				if err != nil {
					return fmt.Errorf("resolve existing compiler input %s: %w", at, err)
				}
				canonical, err := filepath.EvalSymlinks(from)
				if err != nil {
					return fmt.Errorf("resolve compiler input %s: %w", from, err)
				}
				if previous != canonical {
					return fmt.Errorf("compiler input coordinate %q has distinct publications %q and %q. Did you mean to publish the shared module once and depend on it, or keep the emitted closures in separate consumer programs?", filepath.ToSlash(rel), previous, canonical)
				}
				return nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("inspect compiler input destination %s: %w", at, err)
			}
		}
		if err := os.RemoveAll(at); err != nil {
			return err
		}
		return os.Symlink(from, at)
	}
	entries, err := os.ReadDir(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		source := filepath.Join(from, entry.Name())
		if entry.Name() == "node_modules" || rootAbs == source ||
			strings.HasPrefix(rootAbs, source+string(filepath.Separator)) {
			continue
		}
		st, err := os.Stat(source)
		if err != nil {
			return err
		}
		if err := overlayPath(root, rootAbs, source,
			filepath.Join(rel, entry.Name()), st, sourceFiles, replaceManifest, editor); err != nil {
			return err
		}
	}
	return nil
}

// linkAt links the exec root's rel into root at rel, its directories real.
func linkAt(root, execroot, rel string) error {
	at := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		return err
	}
	return os.Symlink(filepath.Join(execroot, rel), at)
}
