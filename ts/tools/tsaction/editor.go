package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

type editorProjection struct {
	*actionConfig
	root, workspace string
	outputContext   bool
	compilerOptions map[string]any
}

func editorRun(root string, command []string, action actionConfig, output string, env ...string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	canonicalCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	a := editorProjection{actionConfig: &action, root: filepath.Join(cwd, root), workspace: cwd}
	project, destination := a.project, a.editorPath
	if output == "" || destination == "" {
		return errors.New("editor-config requires editor-out and editor-path")
	}
	for _, inputs := range [][]string{a.generatedFiles, a.generatedDirectories} {
		for _, input := range inputs {
			if strings.HasPrefix(input, "../") {
				return fmt.Errorf("generated editor project %s does not support external generated input %s: use an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, input)
			}
		}
	}
	var chain *tsconfig.Resolved
	if project != "" {
		var err error
		chain, err = tsconfig.Resolve(project)
		if err != nil {
			return err
		}
		for _, config := range chain.Configs {
			if strings.HasPrefix(path.Clean(config), "external/") {
				return fmt.Errorf("generated editor project %s does not support external tsconfig %s in the extends chain of %s: use an authored editor project or ts_refresh_tsconfig without generated_sources; ordinary builds and type checking remain supported", destination, config, project)
			}
		}
	}
	var stdout bytes.Buffer
	runErr := runToolIn(root, &stdout, command, env...)
	listing, err := explainfiles.Parse(stdout.String())
	if err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || exit.ExitCode() != 1 || len(listing.Diagnostics) == 0 {
			return fmt.Errorf("editor compiler listing: %w\n%s", runErr, stdout.String())
		}
	}
	if len(listing.Diagnostics) != 0 {
		fmt.Fprintln(os.Stderr, strings.Join(listing.Diagnostics, "\n"))
	}
	raw, err := os.ReadFile(a.out)
	if err != nil {
		return err
	}
	var compiled tsconfigFile
	if err := json.Unmarshal(raw, &compiled); err != nil {
		return err
	}
	a.compilerOptions = compiled.CompilerOptions
	rootAbs := a.root
	workspacePath := func(file string) string {
		if !filepath.IsAbs(file) {
			file = filepath.Join(rootAbs, file)
		}
		file = filepath.Clean(file)
		if rel, ok := belowLinkedRoot(rootAbs, file); ok {
			return rel
		}
		return file
	}
	sources, err := a.compilerSourceIdentities(listing.Files, workspacePath)
	if err != nil {
		return err
	}
	listedPath := func(file string) string {
		file = workspacePath(file)
		if source := sources[file]; source != "" {
			return source
		}
		return throughRoot(rootAbs, cwd, canonicalCwd, file)
	}
	for _, listed := range listing.Files {
		file := path.Clean(listedPath(listed))
		if strings.HasPrefix(file, "external/") && a.authored(file) {
			return fmt.Errorf("generated editor project %s does not support external authored input %s: use an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, file)
		}
	}
	types, typesSet := compiled.CompilerOptions["types"].([]any)
	// An explicit empty types list disables discovery, but source directives
	// can still consult typeRoots. Their successes and misses are compiler facts.
	unusedTypeRoots := typesSet && len(types) == 0 &&
		!slices.ContainsFunc(listing.Edges, func(edge explainfiles.Edge) bool { return edge.Kind == explainfiles.TypeReference }) &&
		!slices.ContainsFunc(listing.Unresolved, func(missing explainfiles.Unresolved) bool { return missing.Kind == explainfiles.TypeReference })
	roots, rootsSet := compiled.CompilerOptions["typeRoots"].([]any)
	if !rootsSet {
		checkGeneratedType := func(entry, selected string) error {
			if isRelative(entry) || path.IsAbs(entry) {
				return nil
			}
			file := path.Clean(listedPath(selected))
			source := strings.TrimPrefix(file, a.binDir+"/")
			if a.actionConfig.editorGeneratedPath(source) != source && !a.authored(file) {
				return fmt.Errorf("generated editor project %s: type package %q uses generated input %s with default compilerOptions.typeRoots, which cannot exclude stale checkout packages. Set explicit typeRoots and generate the whole type root, or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, entry, file)
			}
			return nil
		}
		for _, entries := range [][]explainfiles.TypeEntry{listing.Types, listing.Implicit} {
			for _, entry := range entries {
				if err := checkGeneratedType(entry.Entry, entry.File); err != nil {
					return err
				}
			}
		}
		for _, edge := range listing.Edges {
			if edge.Kind == explainfiles.TypeReference {
				if err := checkGeneratedType(edge.Specifier, edge.To); err != nil {
					return err
				}
			}
		}
		for _, missing := range listing.Unresolved {
			if missing.Kind == explainfiles.TypeReference {
				for _, candidate := range missing.Candidates {
					if err := checkGeneratedType(missing.Specifier, candidate.File); err != nil {
						return err
					}
				}
			}
		}
	} else {
		for _, value := range roots {
			root := value.(string)
			if !path.IsAbs(root) {
				root = path.Join(path.Dir(a.out), root)
			}
			if a.editorGeneratedPath(root) != root {
				checkAuthoredType := func(entry, selected string) error {
					file := listedPath(selected)
					if _, contained := below(root, file); contained && a.authored(file) {
						return fmt.Errorf("generated editor project %s: compilerOptions.typeRoots entry %s contains authored type package %q at %s; relocating the root would hide its compiler-selected identity. Use separate authored and generated type roots or an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, root, entry, file)
					}
					return nil
				}
				for _, entries := range [][]explainfiles.TypeEntry{listing.Types, listing.Implicit} {
					for _, entry := range entries {
						if err := checkAuthoredType(entry.Entry, entry.File); err != nil {
							return err
						}
					}
				}
				for _, edge := range listing.Edges {
					if edge.Kind == explainfiles.TypeReference {
						if err := checkAuthoredType(edge.Specifier, edge.To); err != nil {
							return err
						}
					}
				}
				continue
			}
			if unusedTypeRoots {
				continue
			}
			for _, artifacts := range [][]string{a.generatedDirectories, a.generatedFiles} {
				for _, artifact := range artifacts {
					if _, contains := below(root, artifact); contains {
						return fmt.Errorf("generated editor project %s: compilerOptions.typeRoots entry %s contains generated artifact %s but cannot exclude stale checkout packages; generate the whole type root or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, root, artifact)
					}
				}
			}
		}
	}
	config := a.editorConfig(&compiled, chain)
	options := config["compilerOptions"].(map[string]any)
	editorRoot := func(listed string) (string, bool) {
		file := strings.TrimPrefix(listed, a.binDir+"/")
		entry := a.editorGeneratedPath(file)
		generated := entry != file
		if listed != file {
			entry = listed
			generated = a.actionConfig.editorGeneratedPath(file) != file
		}
		if !path.IsAbs(entry) {
			entry = fileRelative(path.Dir(destination), entry)
		}
		return entry, generated
	}
	files := []string{}
	loadedRoots := make(map[string]bool, len(listing.Roots))
	for _, root := range listing.Roots {
		listed := listedPath(root)
		loadedRoots[listed] = true
		entry, _ := editorRoot(listed)
		if !slices.Contains(files, entry) {
			files = append(files, entry)
		}
	}
	for _, root := range compiled.Files {
		if !path.IsAbs(root) {
			root = path.Join(path.Dir(a.out), root)
		}
		listed := listedPath(root)
		_, generated := editorRoot(listed)
		if generated && !loadedRoots[listed] {
			return fmt.Errorf("generated editor project %s: explicit generated root %s has no exact compiler-loaded identity; use the selected filename and extension in files, restore the generated input, or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, root)
		}
	}
	config["files"] = files
	for _, edge := range listing.PackageTargets {
		from, target := listedPath(edge.From), workspacePath(edge.To)
		original := strings.TrimPrefix(from, a.binDir+"/")
		if underNodeModules(from) || a.editorGeneratedPath(target) == target ||
			original != from && a.actionConfig.editorGeneratedPath(original) == from {
			continue
		}
		return fmt.Errorf("generated editor project %s: package import %q from %s reaches generated output %s through a checkout path and could read a stale checkout twin. Use a compilerOptions.paths alias into the generated output or an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, edge.Specifier, from, target)
	}
	for _, missing := range listing.Unresolved {
		if underNodeModules(listedPath(missing.From)) || !editorRelativeReference(missing.Kind, missing.Specifier) {
			continue
		}
		candidates := missing.Candidates
		if candidates == nil || missing.Kind.ModuleSpecifier() {
			candidates = append(slices.Clone(candidates), explainfiles.FailedLookup{File: missing.Specifier})
		}
		if err := a.checkEditorReferenceCandidates(missing.Kind, missing.From, missing.Specifier, candidates, listedPath); err != nil {
			return err
		}
	}
	for _, resolved := range listing.Resolutions {
		if err := a.checkEditorReferenceCandidates(resolved.Kind, resolved.From, resolved.Specifier, resolved.Candidates, listedPath); err != nil {
			return err
		}
	}
	for i := range listing.Edges {
		edge := &listing.Edges[i]
		edge.From = listedPath(edge.From)
		edge.To = listedPath(edge.To)
		if editorRelativeReference(edge.Kind, edge.Specifier) && !underNodeModules(edge.From) {
			direct := path.Join(path.Dir(edge.From), edge.Specifier)
			toOutput := strings.HasPrefix(edge.To, a.binDir+"/")
			if strings.HasPrefix(edge.From, a.binDir+"/") != toOutput || strings.HasPrefix(direct, a.binDir+"/") != toOutput {
				return fmt.Errorf("generated editor project %s: relative import %q from %s resolves to %s across the authored/output namespace boundary, which cannot preserve resolution after output relocation or exclude stale checkout twins. Use a compilerOptions.paths alias for this edge or an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, edge.Specifier, edge.From, edge.To)
			}
		}
	}
	if project != "" && options["paths"] != nil {
		paths := options["paths"].(*tsconfig.Paths)
		if err := a.projectGeneratedAliases(paths, chain, path.Dir(destination), listing.Edges); err != nil {
			return fmt.Errorf("generated editor project %s: %w", destination, err)
		}
		for _, missing := range listing.Unresolved {
			from := listedPath(missing.From)
			if !missing.Kind.ModuleSpecifier() || isRelative(missing.Specifier) || path.IsAbs(missing.Specifier) || underNodeModules(from) {
				continue
			}
			if err := a.checkEditorAliasBoundary(paths, path.Dir(destination), missing.Specifier); err != nil {
				return fmt.Errorf("generated editor project %s: unresolved alias %q from %s: %w", destination, missing.Specifier, from, err)
			}
			if err := a.checkEditorAliasCandidates(paths, missing.Kind, missing.From, missing.Specifier, missing.Candidates, listedPath); err != nil {
				return err
			}
		}
		suffixes, _ := compiled.CompilerOptions["moduleSuffixes"].([]any)
		implicit := a.editorImplicitSelections(listing.Resolutions, chain, listedPath)
		if err := a.projectEditorEdges(paths, chain, path.Dir(destination), listing.Edges, implicit, a.moduleResolution(), suffixes); err != nil {
			return fmt.Errorf("generated editor project %s: %w", destination, err)
		}
		for _, resolved := range listing.Resolutions {
			if err := a.checkEditorAliasCandidates(paths, resolved.Kind, resolved.From, resolved.Specifier, resolved.Candidates, listedPath); err != nil {
				return err
			}
		}
	}
	containment := []string{}
	for _, listed := range listing.Files {
		file := path.Clean(listedPath(listed))
		original := strings.TrimPrefix(file, a.binDir+"/")
		projected := a.editorGeneratedPath(original)
		if original != file {
			projected = file
		}
		generated := projected != original && a.actionConfig.editorGeneratedPath(original) != original
		if generated || a.authored(file) {
			if err := a.checkEditorPackageScope(original); err != nil {
				return fmt.Errorf("generated editor project %s: %w", destination, err)
			}
		}
		if !generated {
			continue
		}
		if !editorNeedsContainment(file, &compiled, path.Dir(a.out)) {
			continue
		}
		outDir, outDirSet := options["outDir"]
		declarationDir, declarationDirSet := options["declarationDir"]
		// Without output-to-source mapping, extra containment cannot change resolution.
		outputMappingDisabled := outDirSet && outDir == nil && declarationDirSet && declarationDir == nil
		if !outputMappingDisabled && !slices.ContainsFunc(listing.Roots, func(root string) bool { return path.Clean(listedPath(root)) == file }) {
			return fmt.Errorf("generated editor project %s: cannot establish rootDir containment for generated non-root source %s from the compiler listing, which does not report external-library membership. Declare this generated source as a program root or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", destination, file)
		}
		containment = append(containment, fileRelative(path.Dir(destination), projected))
	}
	config["rootDirSources"] = containment
	return writeJSON(output, config)
}

func (a *editorProjection) compilerSourceIdentities(files []string, workspacePath func(string) string) (map[string]string, error) {
	selected := map[string]string{}
	for _, file := range files {
		if file = workspacePath(file); filepath.IsAbs(file) {
			selected[file] = ""
		}
	}
	if len(selected) == 0 {
		return selected, nil
	}
	for _, source := range a.srcs {
		physical, err := filepath.EvalSymlinks(filepath.Join(a.workspace, filepath.FromSlash(source)))
		if err != nil {
			return nil, fmt.Errorf("generated editor project %s: resolving authored input %s: %w", a.editorPath, source, err)
		}
		if previous, reported := selected[physical]; reported {
			if previous != "" && previous != source {
				return nil, fmt.Errorf("generated editor project %s: compiler-selected file %s has ambiguous authored input identities %s and %s; retain one source identity or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", a.editorPath, physical, previous, source)
			}
			selected[physical] = source
		}
	}
	return selected, nil
}

func editorNeedsContainment(file string, config *tsconfigFile, directory string) bool {
	if slices.ContainsFunc(declarationSuffixes, func(suffix string) bool { return strings.HasSuffix(file, suffix) }) ||
		config.CompilerOptions["noEmitForJsFiles"] == true && isJavaScript(file) {
		return false
	}
	if path.Ext(file) != ".json" {
		return true
	}
	outDir, _ := config.CompilerOptions["outDir"].(string)
	if outDir == "" {
		return false
	}
	rootDir, _ := config.CompilerOptions["rootDir"].(string)
	absolute := func(value string) string {
		if path.IsAbs(value) {
			return path.Clean(value)
		}
		return path.Join(directory, value)
	}
	return absolute(outDir) != absolute(rootDir)
}

func (a *editorProjection) moduleResolution() string {
	if resolution, _ := a.compilerOptions["moduleResolution"].(string); resolution != "" {
		return resolution
	}
	// --showConfig omits the native compiler's bundler default.
	return "bundler"
}

func (a *editorProjection) activePackageMap(metadata map[string]json.RawMessage) string {
	for _, field := range []string{"imports", "exports"} {
		if field == "exports" {
			// Disabling external exports does not disable package self-references.
			var name *string
			if json.Unmarshal(metadata["name"], &name) != nil || name == nil {
				continue
			}
		} else if a.moduleResolution() == "bundler" && a.compilerOptions["resolvePackageJsonImports"] == false {
			continue
		}
		if value := metadata[field]; len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			var entries map[string]json.RawMessage
			if json.Unmarshal(value, &entries) == nil && len(entries) == 0 {
				continue
			}
			return field
		}
	}
	return ""
}

func (a *editorProjection) checkEditorPackageScope(file string) error {
	scope := ""
	var raw []byte
	for directory := path.Dir(file); ; directory = path.Dir(directory) {
		scope = path.Join(directory, "package.json")
		selected := a.editorGeneratedPath(scope)
		var err error
		raw, err = os.ReadFile(filepath.Join(a.root, filepath.FromSlash(selected)))
		if err == nil {
			if selected != scope {
				if a.authored(file) {
					return fmt.Errorf("generated editor project %s: generated package scope %s has a mixed package namespace containing authored input %s. Relocation would change imports or module format, including unobserved queries. Generate the complete package namespace without authored members or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", a.editorPath, scope, file)
				}
				return a.checkPackageNamespace(selected, scope)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if directory == path.Dir(directory) {
			return nil
		}
	}
	if a.authored(file) {
		return nil
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("reading package scope %s: %w", scope, err)
	}
	if field := a.activePackageMap(metadata); field != "" {
		return fmt.Errorf("generated input %s is enclosed by package scope %s with %s: relocating its package-relative resolution context cannot preserve unobserved imports or self-references. Keep the complete package namespace inside generated output or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", file, scope, field)
	}
	// A scope in the output tree is its owner's as-built manifest; a non-owner's rewrite looks the same.
	staged := path.Join(a.binDir, scope)
	_, err := os.Stat(staged)
	if err == nil {
		return nil
	}
	// An editor walking up from the output tree reaches the workspace root's own scope.
	if scope == "package.json" && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("generated input %s requires its owner's as-built package scope %s at %s: %w", file, scope, staged, errors.Join(err, errors.New("package scope is not preserved")))
}

func (a *editorProjection) checkPackageContext(source, original, projected string) error {
	if !a.outputContext {
		return nil
	}
	_, before := below(filepath.Dir(original), a.out)
	_, after := below(filepath.Dir(projected), a.editorPath)
	if before == after {
		return nil
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("reading package scope %s: %w", source, err)
	}
	if field := a.activePackageMap(metadata); field != "" {
		return fmt.Errorf("generated editor project %s: config placement changes package scope %s for %s while emission output directories are active, including for unobserved queries. Use an editor configuration without emission output directories or an authored editor project without generated_sources; ordinary builds and type checking remain supported", a.editorPath, original, field)
	}
	return nil
}

func (a *editorProjection) checkGeneratedPackageContexts(directory string) error {
	if !a.outputContext {
		return nil
	}
	root := filepath.Join(a.binDir, directory)
	_, before := below(root, a.out)
	_, after := below(root, a.editorPath)
	if !before && !after {
		return nil
	}
	return filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			_, before := below(file, a.out)
			_, after := below(file, a.editorPath)
			if !before && !after {
				return filepath.SkipDir
			}
		} else if entry.Name() == "package.json" {
			return a.checkPackageContext(file, file, file)
		}
		return nil
	})
}

func (a *editorProjection) checkPackageNamespace(source, destination string) error {
	if relative, contained := below(a.workspace, source); contained {
		source = relative
	}
	source, destination = filepath.ToSlash(source), filepath.ToSlash(destination)
	original := strings.TrimPrefix(source, a.binDir+"/")
	if a.actionConfig.editorGeneratedPath(original) == original {
		return a.checkPackageContext(source, destination, destination)
	}
	if source != path.Join(a.binDir, destination) {
		return fmt.Errorf("generated editor project %s: generated package scope %s cannot retain its canonical namespace at %s. Keep the producer manifest at its original package path or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", a.editorPath, source, destination)
	}
	return a.checkPackageContext(source, destination, source)
}

func editorRelativeReference(kind explainfiles.EdgeKind, specifier string) bool {
	return isRelative(specifier) || kind == explainfiles.Reference && !path.IsAbs(specifier)
}

func (a *editorProjection) checkEditorReferenceCandidates(kind explainfiles.EdgeKind, from, specifier string, candidates []explainfiles.FailedLookup, listedPath func(string) string) error {
	from = listedPath(from)
	if underNodeModules(from) || !editorRelativeReference(kind, specifier) {
		return nil
	}
	for _, candidate := range candidates {
		direct := path.Join(path.Dir(from), candidate.File)
		if filepath.IsAbs(candidate.File) {
			direct = listedPath(candidate.File)
		}
		if a.editorGeneratedPath(direct) != direct {
			return fmt.Errorf("generated editor project %s: reference %q from %s failed to resolve generated output %s and could read a stale checkout twin. Use a compilerOptions.paths alias for this edge or an authored editor project without generated_sources; ordinary builds and type checking remain supported", a.editorPath, specifier, from, direct)
		}
	}
	return nil
}

func (a *editorProjection) checkEditorAliasCandidates(paths *tsconfig.Paths, kind explainfiles.EdgeKind, from, specifier string, candidates []explainfiles.FailedLookup, listedPath func(string) string) error {
	from = listedPath(from)
	if !kind.ModuleSpecifier() || isRelative(specifier) || path.IsAbs(specifier) || underNodeModules(from) {
		return nil
	}
	for _, candidate := range candidates {
		file := listedPath(candidate.File)
		if candidate.Candidate == "" || !slices.Contains(a.generatedFiles, file) || a.authored(file) {
			continue
		}
		lookup := path.Join(path.Dir(a.out), candidate.Candidate)
		if path.IsAbs(candidate.Candidate) {
			lookup = listedPath(candidate.Candidate)
		}
		alias, middle := editorAliasOwner(paths, specifier)
		for _, value := range paths.Get(alias) {
			value = strings.Replace(value, "*", middle, 1)
			target := path.Join(path.Dir(a.editorPath), value)
			if path.IsAbs(value) {
				target = listedPath(value)
			}
			if target == lookup {
				return fmt.Errorf("generated editor project %s: alias %q from %s failed to resolve generated output %s but retains checkout candidate %s, which could read a stale checkout twin. Use canonical output paths or an authored editor project without generated_sources; ordinary builds and type checking remain supported", a.editorPath, specifier, from, file, target)
			}
		}
	}
	return nil
}

func (a *editorProjection) editorImplicitSelections(resolutions []explainfiles.Resolution, chain *tsconfig.Resolved, listedPath func(string) string) map[explainfiles.Edge]bool {
	// The compiler selects the substitution; extension and suffix probing stay native.
	paths := a.paths(chain, path.Dir(a.out))
	native := *a.actionConfig
	native.binDir = ""
	authored := native.paths(chain, path.Dir(a.out))
	selections := map[explainfiles.Edge]bool{}
	for _, resolution := range resolutions {
		if resolution.FromPackage {
			continue
		}
		edge := resolution.Edge
		edge.From, edge.To = listedPath(edge.From), listedPath(edge.To)
		selected := resolution.To
		if relative, ok := below(a.root, selected); ok {
			selected = relative
		}
		overlaid := selected != edge.To && a.editorGeneratedPath(selected) == edge.To
		owner, _ := editorAliasOwner(chain.Paths, resolution.Specifier)
		implicit := false
		for _, substitution := range resolution.Substitutions {
			index := 0
			for _, value := range authored.Get(owner) {
				count := 1
				if !path.IsAbs(value) {
					count = 2
				}
				matched := false
				for offset := 0; offset < count; offset++ {
					if substitution == paths.Get(owner)[index+offset] {
						implicit, matched = offset == 1 || overlaid, true
						break
					}
				}
				if matched {
					break
				}
				index += count
			}
		}
		if implicit {
			selections[edge] = true
		}
	}
	return selections
}

func (a *editorProjection) projectEditorEdges(paths *tsconfig.Paths, chain *tsconfig.Resolved, directory string, edges []explainfiles.Edge, implicit map[explainfiles.Edge]bool, resolution string, suffixes []any) error {
	selections := map[string]map[explainfiles.Edge]bool{}
	for _, edge := range edges {
		if !edge.Kind.ModuleSpecifier() || isRelative(edge.Specifier) || path.IsAbs(edge.Specifier) || underNodeModules(edge.From) {
			continue
		}
		owner, _ := editorAliasOwner(chain.Paths, edge.Specifier)
		if owner == "" {
			continue
		}
		if selections[edge.Specifier] == nil {
			selections[edge.Specifier] = map[explainfiles.Edge]bool{}
		}
		selections[edge.Specifier][edge] = true
	}
	specifiers := make([]string, 0, len(selections))
	for specifier := range selections {
		specifiers = append(specifiers, specifier)
	}
	slices.Sort(specifiers)
	for _, specifier := range specifiers {
		importers := selections[specifier]
		selected := ""
		consistent := true
		owner, middle := editorAliasOwner(chain.Paths, specifier)
		candidates, _ := a.projectEditorCandidates(chain, owner, middle, directory)
		needsProjection := false
		exact := false
		for edge := range importers {
			file := path.Clean(edge.To)
			selection := edge
			selection.Kind = explainfiles.Import
			generated, canonical := strings.CutPrefix(file, a.binDir+"/")
			authoredInTree := a.authored(file) && a.actionConfig.editorGeneratedPath(file) != file
			if authoredInTree || canonical && slices.Contains(a.generatedFiles, generated) && implicit[selection] {
				needsProjection = true
				exact = exact || !slices.Contains(candidates, fileRelative(directory, file))
			}
			if selected != "" && selected != file {
				consistent = false
			}
			selected = file
		}
		if !needsProjection {
			continue
		}
		if !exact {
			paths.Set(specifier, candidates)
			continue
		}
		for _, suffix := range suffixes {
			if suffix != "" {
				return fmt.Errorf("alias %q needs exact file projection with compilerOptions.moduleSuffixes=%v: the compiler applies suffixes again to the selected filename, so the exact path cannot preserve its selection. Use an authored editor project or ts_refresh_tsconfig without generated_sources; ordinary builds and type checking remain supported", specifier, suffixes)
			}
		}
		if !consistent {
			resolutions := make([]string, 0, len(importers))
			for edge := range importers {
				resolutions = append(resolutions, edge.From+" -> "+edge.To)
			}
			slices.Sort(resolutions)
			return fmt.Errorf("alias %q resolves to different files (%s); one paths entry cannot represent these importer or resolution-mode choices. Use distinct aliases, separate editor projects, or an authored editor project without generated_sources; ordinary builds and type checking remain supported", specifier, strings.Join(resolutions, "; "))
		}
		if resolution == "node16" || resolution == "nodenext" {
			return fmt.Errorf("first-party alias %q cannot be projected with compilerOptions.moduleResolution=%q: exact paths can hide failed imports in another importer's resolution mode. Use an authored editor project or ts_refresh_tsconfig without generated_sources; ordinary builds and type checking remain supported", specifier, resolution)
		}
		paths.Set(specifier, []string{fileRelative(directory, selected)})
	}
	return nil
}

func (a *editorProjection) authored(value string) bool {
	return slices.Contains(a.srcs, path.Clean(filepath.ToSlash(value)))
}

func (a *editorProjection) editorGeneratedPath(value string) string {
	if a.authored(value) {
		return value
	}
	return a.actionConfig.editorGeneratedPath(value)
}

func (a *actionConfig) editorGeneratedPath(value string) string {
	for _, file := range a.generatedFiles {
		if value == file {
			return path.Join(a.binDir, value)
		}
	}
	for _, directory := range a.generatedDirectories {
		if value == directory || strings.HasPrefix(value, directory+"/") {
			return path.Join(a.binDir, value)
		}
	}
	return value
}

func (a *editorProjection) editorConfig(config *tsconfigFile, chain *tsconfig.Resolved) map[string]any {
	dir := path.Dir(a.editorPath)
	remap := a.editorGeneratedPath
	relocate := func(values []string, mapPath func(string) string) []string {
		out := make([]string, 0, len(values))
		for _, value := range values {
			if path.IsAbs(value) {
				out = append(out, value)
				continue
			}
			target := path.Join(path.Dir(a.out), value)
			if mapPath != nil {
				target = mapPath(target)
			}
			out = append(out, fileRelative(dir, target))
		}
		return out
	}
	opts := map[string]any{"noEmit": true, "moduleResolution": a.moduleResolution()}
	for _, key := range []string{"composite", "incremental", "declaration", "declarationMap", "emitDeclarationOnly", "declarationDir", "isolatedDeclarations", "types", "allowJs", "skipLibCheck"} {
		if value, ok := config.CompilerOptions[key]; ok {
			opts[key] = value
		}
	}
	opts["rootDir"] = relativePath(dir, "")
	opts["rootDirs"] = []string{}
	opts["outDir"] = nil
	if outDir, ok := config.CompilerOptions["outDir"].(string); ok && outDir != "" {
		opts["outDir"] = relocate([]string{outDir}, nil)[0]
	}
	if value, ok := config.CompilerOptions["typeRoots"].([]any); ok {
		roots := make([]string, 0, len(value))
		for _, root := range value {
			roots = append(roots, root.(string))
		}
		opts["typeRoots"] = relocate(roots, remap)
	}
	if chain != nil {
		projected := *a.actionConfig
		projected.binDir = ""
		if paths := projected.paths(chain, dir); paths != nil {
			opts["paths"] = paths
		}
	}
	return map[string]any{"_rules_typescript": "compiler-project", "extends": a.extends(dir), "compilerOptions": opts, "include": []string{}, "exclude": []string{}, "references": []string{}}
}

func (a *editorProjection) projectEditorCandidates(chain *tsconfig.Resolved, alias, middle, dir string) ([]string, bool) {
	projected := []string{}
	changed := false
	for _, value := range chain.Paths.Get(alias) {
		value = strings.Replace(value, "*", middle, 1)
		if path.IsAbs(value) {
			projected = append(projected, value)
			continue
		}
		target := path.Join(chain.PathsDir, value)
		generated := a.editorGeneratedPath(target)
		projected = append(projected, fileRelative(dir, generated))
		changed = changed || a.actionConfig.editorGeneratedPath(target) != target
	}
	return projected, changed
}

func (a *editorProjection) projectGeneratedAliases(out *tsconfig.Paths, chain *tsconfig.Resolved, dir string, edges []explainfiles.Edge) error {
	keys := &tsconfig.Paths{}
	for alias := range chain.Paths.Entries() {
		keys.Set(alias, nil)
	}
	for alias, values := range chain.Paths.Entries() {
		for _, value := range values {
			if path.IsAbs(value) {
				continue
			}
			target := path.Join(chain.PathsDir, value)
			if strings.Count(alias, "*") != 1 || strings.Count(target, "*") != 1 {
				continue
			}
			parts := strings.SplitN(target, "*", 2)
			for _, generated := range a.generatedDirectories {
				if strings.HasPrefix(generated+"/", parts[0]) {
					keys.Set(strings.Replace(alias, "*", strings.TrimPrefix(generated+"/", parts[0])+"*", 1), nil)
				}
				if strings.HasPrefix(generated, parts[0]) {
					middle := strings.TrimPrefix(generated, parts[0])
					for end := 0; end <= len(middle); end++ {
						if strings.HasPrefix(parts[1], middle[end:]) {
							keys.Set(strings.Replace(alias, "*", middle[:end], 1), nil)
						}
					}
				}
			}
		}
	}
	for key := range keys.Entries() {
		selected, middle := editorAliasOwner(chain.Paths, key)
		projected, changed := a.projectEditorCandidates(chain, selected, middle, dir)
		if changed {
			if chain.Paths.Index(key) < 0 && strings.Contains(key, "*") {
				prefix, suffix, _ := strings.Cut(key, "*")
				ownerPrefix, _, _ := strings.Cut(selected, "*")
				for alias := range chain.Paths.Entries() {
					authoredPrefix, authoredSuffix, pattern := strings.Cut(alias, "*")
					// A suffix pattern can overlap a synthetic wildcard without
					// matching the literal '*' used to construct that wildcard.
					higherAuthored := len(authoredPrefix) > len(ownerPrefix) || len(authoredPrefix) == len(ownerPrefix) && chain.Paths.Index(alias) < chain.Paths.Index(selected)
					higherSynthetic := len(prefix) > len(authoredPrefix)
					if pattern && authoredSuffix != "" && higherAuthored && higherSynthetic && strings.HasPrefix(prefix, authoredPrefix) && (strings.HasSuffix(suffix, authoredSuffix) || strings.HasSuffix(authoredSuffix, suffix)) {
						return fmt.Errorf("generated alias %q overlaps higher-priority authored suffix alias %q: one paths pattern cannot retain both candidate lists for the overlap. Use disjoint aliases or an authored editor project without generated_sources; ordinary builds and type checking remain supported", key, alias)
					}
				}
			}
			out.Set(key, projected)
		}
	}
	for _, edge := range edges {
		if !edge.Kind.ModuleSpecifier() || isRelative(edge.Specifier) || path.IsAbs(edge.Specifier) || underNodeModules(edge.From) {
			continue
		}
		alias, middle := editorAliasOwner(chain.Paths, edge.Specifier)
		for _, value := range chain.Paths.Get(alias) {
			if path.IsAbs(value) {
				continue
			}
			target := path.Join(chain.PathsDir, strings.Replace(value, "*", middle, 1))
			// An earlier explicit file candidate keeps the observed selection.
			if target == edge.To {
				break
			}
			if slices.Contains(a.generatedDirectories, target) && path.Dir(edge.To) == path.Dir(target) && strings.HasPrefix(edge.To, target+".") {
				return fmt.Errorf("alias %q from %s resolves to authored sibling %s beside directory candidate %s: relocating the generated tree hides this resolution, but retaining the checkout candidate can revive deleted outputs. Name the authored file explicitly in a distinct alias, or use an authored editor project without generated_sources; ordinary builds and type checking remain supported", edge.Specifier, edge.From, edge.To, target)
			}
		}
		if err := a.checkEditorAliasBoundary(out, dir, edge.Specifier); err != nil {
			return fmt.Errorf("alias %q from %s resolves to %s but %w", edge.Specifier, edge.From, edge.To, err)
		}
	}
	return nil
}

func (a *editorProjection) checkEditorAliasBoundary(paths *tsconfig.Paths, dir, specifier string) error {
	alias, middle := editorAliasOwner(paths, specifier)
	for _, directory := range a.generatedDirectories {
		for _, value := range paths.Get(alias) {
			if path.IsAbs(value) {
				continue
			}
			// paths matches the literal specifier before normalizing its substitution.
			target := path.Join(dir, strings.Replace(value, "*", middle, 1))
			if a.authored(target) {
				continue
			}
			output := path.Join(a.binDir, directory)
			_, projected := below(output, path.Join(dir, value))
			_, contained := below(output, target)
			if target == directory || strings.HasPrefix(target, directory+"/") || projected && !contained {
				return fmt.Errorf("its projected alias %q crosses generated directory %s, redirecting an authored path or allowing a stale checkout twin. Use a normalized import such as %q or an authored editor project without generated_sources; ordinary builds and type checking remain supported", alias, directory, path.Clean(specifier))
			}
		}
	}
	return nil
}

func editorAliasOwner(paths *tsconfig.Paths, specifier string) (string, string) {
	if paths.Index(specifier) >= 0 {
		return specifier, "*"
	}
	selected, middle := "", ""
	longest := -1
	for alias := range paths.Entries() {
		if strings.Count(alias, "*") != 1 {
			continue
		}
		parts := strings.SplitN(alias, "*", 2)
		if len(specifier) < len(parts[0])+len(parts[1]) || !strings.HasPrefix(specifier, parts[0]) || !strings.HasSuffix(specifier, parts[1]) {
			continue
		}
		if len(parts[0]) > longest {
			selected = alias
			middle = strings.TrimSuffix(strings.TrimPrefix(specifier, parts[0]), parts[1])
			longest = len(parts[0])
		}
	}
	return selected, middle
}
