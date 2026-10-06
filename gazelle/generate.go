package typescript

import (
	"fmt"
	"log"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	bzl "github.com/bazelbuild/buildtools/build"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// generateRules writes one directory: a package's targets, or the withdrawal
// of what an earlier run left where no program is.
func generateRules(args language.GenerateArgs) language.GenerateResult {
	if isEditorProjectDir(args.Rel) {
		return language.GenerateResult{}
	}
	tc := getConfig(args.Config)
	s := tc.programs
	if err := s.inputs[args.Rel].discoveryErr; err != nil {
		log.Fatal(err)
	}
	s.visit(args.Rel, args.RegularFiles)
	s.walked[args.Rel] = true
	for _, file := range args.RegularFiles {
		s.walked[parentDir(path.Join(args.Rel, file))] = true
	}
	if p := s.programs[args.Rel]; p != nil {
		input := s.inputs[args.Rel]
		input.program = p
		s.inputs[args.Rel] = input
		s.selectInput(args.Rel)
	}
	var res language.GenerateResult
	producer, _, root := s.declaredOutputProducer(args.Rel, true)
	if producer != nil {
		res = codegenOutDirResult(args, root)
	} else {
		res = programRules(args, tc)
		res = withImporterRules(args, tc, res)
	}
	res = withProtoRules(args, tc, res)
	if producer == nil && (s.packages[args.Rel] != nil || s.metadataIdentity(args.Config, tsconfigIn(args.Rel)) != unknownInput) {
		res = withPackageScopeCompiler(args, tc, res)
	}
	if producer == nil && s.packages[args.Rel] == nil {
		reportWithdrawnProgramRules(args, tc, res)
	}
	res = withSourceExports(args, tc, res)
	reportTakenNames(args, res.Gen)
	reportManagedAttrDrops(args, res.Gen, res.Imports...)
	// Resolve runs before Gazelle writes newly generated BUILD files.
	if args.File != nil || len(res.Gen) > 0 || len(args.OtherGen) > 0 {
		s.generatedPackages[args.Rel] = true
	}
	if args.File == nil && s.generatedPackages[args.Rel] {
		s.emission.setFile(args.Rel, &rule.File{
			Path:  filepath.Join(args.Dir, args.Config.DefaultBuildFileName()),
			Pkg:   args.Rel,
			Rules: append(slices.Clone(args.OtherGen), res.Gen...),
		})
		s.observeBuild(args.Rel)
	}
	return res
}

func programRules(args language.GenerateArgs, tc *tsConfig) language.GenerateResult {
	if tc.programs.packages[args.Rel] == nil {
		return nonPackageRules(args, tc)
	}
	return packageRules(args, tc)
}

// Gazelle fixes its Resolve callbacks before completing the native index.
func registerProgramRules(args language.GenerateArgs, res language.GenerateResult) language.GenerateResult {
	registered := res
	registered.Gen, registered.Imports = nil, nil
	for _, owned := range ownedRuleNames(args.Rel) {
		registered.Empty = slices.DeleteFunc(registered.Empty, func(r *rule.Rule) bool {
			return r.Kind() == owned.Kind() && r.Name() == owned.Name()
		})
		i := slices.IndexFunc(res.Gen, func(r *rule.Rule) bool {
			return r.Kind() == owned.Kind() && r.Name() == owned.Name()
		})
		var imports any
		if i >= 0 {
			owned, imports = res.Gen[i], res.Imports[i]
			res.Gen = slices.Delete(res.Gen, i, i+1)
			res.Imports = slices.Delete(res.Imports, i, i+1)
		}
		if managedProgramRule(owned, args.Rel) && imports == nil {
			imports = &ruleImports{}
		}
		registered.Gen = append(registered.Gen, owned)
		registered.Imports = append(registered.Imports, imports)
	}
	registered.Gen = append(registered.Gen, res.Gen...)
	registered.Imports = append(registered.Imports, res.Imports...)
	return registered
}

// withImporterRules adds the lockfile importer's rules for the directory, or
// withdraws them where it is no importer.
func withImporterRules(args language.GenerateArgs, tc *tsConfig,
	res language.GenerateResult) language.GenerateResult {
	var gen []*rule.Rule
	if tc.lock != nil {
		gen = tc.lock.importerRules(args.Rel)
	}
	for _, r := range gen {
		res.Gen = append(res.Gen, r)
		res.Imports = append(res.Imports, nil)
	}
	res.Empty = append(res.Empty, importerEmpties(args.File, gen)...)
	return res
}

// packageName is the ts_compile's name in rel: the directory's basename.
func packageName(rel string) string {
	if rel == "" {
		return "root"
	}
	return path.Base(rel)
}

func devTargetName(name string) string {
	if name == "dev" {
		return "dev_server"
	}
	return "dev"
}

func ownedRuleNames(rel string) []*rule.Rule {
	name := packageName(rel)
	return []*rule.Rule{
		rule.NewRule("ts_compile", name),
		rule.NewRule("ts_test", testTargetName(name)),
		rule.NewRule("ts_dev_server", devTargetName(name)),
		rule.NewRule("ts_config", tsConfigTargetName),
		rule.NewRule("filegroup", vitestConfigTargetName),
		rule.NewRule("filegroup", wranglerConfigTargetName),
	}
}

func emptyResult(args language.GenerateArgs) language.GenerateResult {
	return language.GenerateResult{Empty: ownedRuleNames(args.Rel)}
}

// No program: every rule Gazelle would write is withdrawn and a BUILD file
// holding one named; an extended tsconfig.json and the root's configs stay.
func nonPackageRules(args language.GenerateArgs, tc *tsConfig,
) language.GenerateResult {
	switch tc.programs.metadataIdentity(args.Config, tsconfigIn(args.Rel)) {
	case generatedInput:
		log.Fatal(generatedDiscoveryConflict(tsconfigIn(args.Rel)))
	case unknownInput:
		if args.File != nil {
			for _, stored := range args.File.Rules {
				r := canonicalRule(args.Config, stored)
				if managedProgramRule(r, args.Rel) && !r.ShouldKeep() {
					log.Fatal(tc.programs.incompleteOutput(tsconfigIn(args.Rel)))
				}
			}
		}
		log.Printf("%v; automatic program discovery is omitted", tc.programs.incompleteOutput(tsconfigIn(args.Rel)))
		return language.GenerateResult{}
	}
	var res language.GenerateResult
	rootConfig := ""
	if args.Rel == "" {
		rootConfig = exportedVitestConfig(args.Config, tc.programs, args.Dir, "")
	}
	for _, r := range ownedRuleNames(args.Rel) {
		switch {
		case r.Kind() == "ts_config" && tc.programs.extended[args.Rel] &&
			tc.programs.hasAuthoredConfig(args.Config.RepoRoot, args.Rel):
			res.Gen = append(res.Gen, tsConfigRule(args, tc))
			res.Imports = append(res.Imports, nil)
		case r.Kind() == "filegroup" && rootConfig != "":
			if fg := configFilegroup(args, tc, rootConfig, r.Name()); fg != nil {
				res.Gen = append(res.Gen, fg)
				res.Imports = append(res.Imports, nil)
				continue
			}
			fallthrough
		default:
			res.Empty = append(res.Empty, r)
		}
	}
	return res
}

func reportWithdrawnProgramRules(args language.GenerateArgs, tc *tsConfig, res language.GenerateResult) {
	var held []string
	for _, r := range ownedRuleNames(args.Rel) {
		if !slices.ContainsFunc(res.Empty, func(empty *rule.Rule) bool {
			return empty.Kind() == r.Kind() && empty.Name() == r.Name()
		}) {
			continue
		}
		if have := existingRule(args, r.Kind(), r.Name()); have != nil && !have.ShouldKeep() {
			held = append(held, r.Kind()+"("+r.Name()+")")
		}
	}
	if len(held) > 0 {
		log.Printf("typescript: %s: %s is not a package -- %s -- so %s is "+
			"withdrawn; Gazelle cannot delete the file, so delete it by hand "+
			"if nothing else is in it", args.File.Path, orRepoRoot(args.Rel),
			whyNotPackage(tc), strings.Join(held, ", "))
	}
}

func whyNotPackage(tc *tsConfig) string {
	if m := tc.foreignManifest; m != "" {
		return foreignReason(m)
	}
	return "neither tsconfig.json nor package.json source exports here list a first-party file"
}

// packageRules writes a package: ts_compile, ts_test, the declarations in
// both, the tree's other files in the first that exists, its ts_config.
func packageRules(args language.GenerateArgs, tc *tsConfig,
) language.GenerateResult {
	s, pkg := tc.programs, args.Rel
	name := packageName(pkg)
	set := s.srcs(pkg, tc)
	data := s.dataFiles(pkg, tc)
	codegens := codegenLabels(args, s)
	tsConfigAttr := ""
	if !s.programs[pkg].manifest {
		tsConfigAttr = ":" + tsConfigTargetName
	}
	var res language.GenerateResult
	add := func(r *rule.Rule, imports any) {
		if _, ok := imports.(*ruleImports); ok {
			programRuleSources(args, r, set, data)
		}
		res.Gen = append(res.Gen, r)
		res.Imports = append(res.Imports, imports)
	}
	withdraw := func(kind, name string) {
		res.Empty = append(res.Empty, rule.NewRule(kind, name))
	}

	nodeModules := ""
	if tc.lock != nil {
		nodeModules = tc.lock.nodeModulesLabel(pkg)
	}
	compile := len(set.library) > 0
	if compile {
		r := rule.NewRule("ts_compile", name)
		if tsConfigAttr != "" {
			r.SetAttr("tsconfig", tsConfigAttr)
		}
		if nodeModules != "" {
			r.SetAttr("node_modules", nodeModules)
		}
		r.SetAttr("visibility", []string{"//visibility:public"})
		imps := &ruleImports{}
		imps.deps = append(imps.deps, codegens...)
		add(r, imps)
	} else {
		withdraw("ts_compile", name)
	}

	if compile && tc.programs.appPackage(args.Config, args.Rel, set.library) {
		r := rule.NewRule("ts_dev_server", devTargetName(name))
		r.SetAttr("entry_point", ":"+name)
		r.SetAttr("plugin", "@rules_typescript//vite:vite_plugin_bazel")
		if nodeModules != "" {
			r.SetAttr("node_modules", nodeModules)
		}
		r.SetAttr("visibility", []string{"//visibility:public"})
		add(r, nil)
	} else {
		withdraw("ts_dev_server", devTargetName(name))
	}

	if len(set.test) > 0 {
		r := rule.NewRule("ts_test", testTargetName(name))
		attr, cfg := vitestConfigFor(args, tc)
		if attr != "" {
			r.SetAttr("config", attr)
		}
		if tsConfigAttr != "" {
			r.SetAttr("tsconfig", tsConfigAttr)
		}
		if nodeModules != "" {
			r.SetAttr("node_modules", nodeModules)
		}
		compileLabel := ""
		if compile {
			compileLabel = ":" + name
		}
		imps := s.testImports(tc.lock, pkg, compileLabel,
			cfg)
		imps.deps = append(imps.deps, codegens...)
		add(r, imps)
	} else {
		withdraw("ts_test", testTargetName(name))
	}
	if !compile && len(set.test) == 0 {
		s.say("%s: the program lists only declaration files, so no target compiles them", s.configPath(pkg))
	}

	if tsConfigAttr != "" {
		add(tsConfigRule(args, tc), nil)
	} else {
		withdraw("ts_config", tsConfigTargetName)
	}
	cfgName := exportedVitestConfig(args.Config, tc.programs, args.Dir, pkg)
	for _, fg := range []string{vitestConfigTargetName, wranglerConfigTargetName} {
		var r *rule.Rule
		if cfgName != "" {
			r = configFilegroup(args, tc, cfgName, fg)
		}
		if r != nil {
			add(r, nil)
		} else {
			withdraw("filegroup", fg)
		}
	}
	return res
}

func programRuleSources(args language.GenerateArgs, r *rule.Rule, set srcSet, data []string) {
	sources := set.library
	if canonicalRule(args.Config, r).Kind() == "ts_test" {
		sources = set.test
		if len(set.library) > 0 {
			data = nil
		}
	}
	r.SetAttr("srcs", packageSrcs(args, sources, set.declaration, data))
}

func managedProgramRule(r *rule.Rule, pkg string) bool {
	return r.Kind() == "ts_compile" && r.Name() == packageName(pkg) ||
		r.Kind() == "ts_test" && r.Name() == testTargetName(packageName(pkg))
}

func (s *programStore) keptProgramSources(c *config.Config, pkg string, dirInfo ...func(string) (walk.DirInfo, error)) []string {
	if s.emission == nil || s.emission.files[pkg] == nil {
		return nil
	}
	file := s.emission.files[pkg]
	var sources []string
	for _, stored := range file.Rules {
		r := canonicalRule(c, stored)
		if !managedProgramRule(r, pkg) {
			continue
		}
		for _, spec := range automaticProgramImports(c, r, keptSourceRule(r, file), file, dirInfo...) {
			sources = append(sources, spec.Imp)
		}
	}
	return sources
}

func refreshProgramRules(args language.GenerateArgs, tc *tsConfig, res language.GenerateResult) {
	s := tc.programs
	final := programRules(args, tc)
	if s.packages[args.Rel] != nil || s.metadataIdentity(args.Config, tsconfigIn(args.Rel)) != unknownInput {
		final = withPackageScopeCompiler(args, tc, final)
	}
	reportTakenNames(args, final.Gen)
	kinds := (&tsLang{}).Kinds()
	ownedRules := ownedRuleNames(args.Rel)
	discard := func(r *rule.Rule) {
		r.Delete()
		if file := s.emission.files[args.Rel]; file != nil && file.File == nil {
			file.Rules = slices.DeleteFunc(file.Rules, func(stored *rule.Rule) bool { return stored == r })
			s.ruleListsMayChange()
		}
	}
	var library *rule.Rule
	for i, generated := range res.Gen {
		r := canonicalRule(args.Config, generated)
		if !slices.ContainsFunc(ownedRules, func(owned *rule.Rule) bool {
			return owned.Kind() == r.Kind() && owned.Name() == r.Name()
		}) {
			continue
		}
		j := slices.IndexFunc(final.Gen, func(want *rule.Rule) bool {
			return want.Kind() == r.Kind() && want.Name() == r.Name()
		})
		want := rule.NewRule(r.Kind(), r.Name())
		if j >= 0 {
			want = final.Gen[j]
		}
		effective := existingRule(args, r.Kind(), r.Name())
		key := emissionLabel(args.Config.RepoName, args.Rel, ":"+r.Name())
		if effective == nil {
			if holder := s.semanticRule(key); holder != nil && holder.Kind() != r.Kind() {
				for _, attr := range generated.AttrKeys() {
					if attr != "name" {
						generated.DelAttr(attr)
					}
				}
				discard(generated)
				continue
			}
			effective = generated
		}
		info := kinds[r.Kind()]
		attrs := maps.Clone(info.MergeableAttrs)
		maps.Copy(attrs, info.ResolveAttrs)
		rule.MergeRules(want, generated, attrs, args.Dir)
		if effective != generated {
			rule.MergeRules(want, effective, attrs, args.Dir)
		}
		scopeCandidate := want.PrivateAttr("_ts_scope_candidate") == true
		generated.SetPrivateAttr("_ts_scope_candidate", scopeCandidate)
		effective.SetPrivateAttr("_ts_scope_candidate", scopeCandidate)
		if j < 0 {
			discard(generated)
			if !effective.ShouldKeep() && effective.IsEmpty(info) {
				discard(effective)
				s.emission.deleteRule(key)
				continue
			}
		}
		s.emission.setRule(key, effective)
		if imps, ok := res.Imports[i].(*ruleImports); ok {
			reportSrcDrops := imps.reportSrcDrops
			*imps = ruleImports{}
			if j >= 0 {
				if selected, ok := final.Imports[j].(*ruleImports); ok {
					*imps = *selected
				}
			}
			imps.reportSrcDrops = reportSrcDrops
			if scopeCandidate || effective.IsEmpty(info) {
				continue
			}
			sourceRules := []*rule.Rule{effective}
			if r.Kind() == "ts_compile" {
				library = effective
			} else if library != nil {
				cfg := compilerConfigSource(args.Config, effective.AttrString("tsconfig"), args.Rel)
				if cfg != "" && cfg == compilerConfigSource(args.Config, library.AttrString("tsconfig"), args.Rel) {
					sourceRules = append(sourceRules, library)
				}
			}
			file := args.File
			if file == nil {
				file = &rule.File{Pkg: args.Rel}
			}
			var sources []string
			for _, sourceRule := range sourceRules {
				for _, spec := range automaticProgramImports(args.Config, effective, sourceRule, file) {
					sources = append(sources, spec.Imp)
				}
			}
			for _, source := range sources {
				if programCandidate(source) && !s.generatedProgramFile(args.Config, s.index, source, true) {
					s.requireAuthoredScope(args.Config, tsconfigIn(args.Rel), source)
				}
			}
			imps.program = s.compilerProgram(args.Config, effective.AttrString("tsconfig"), args.Rel)
			if imps.program == nil {
				if p := s.programs[args.Rel]; p != nil && p.manifest && effective.Attr("tsconfig") == nil {
					imps.program = p
				}
			}
			if managesProgramClosure(effective) && !effective.IsEmpty(kinds[r.Kind()]) {
				if err := compilerClosureError(imps.program, label.New(args.Config.RepoName, args.Rel, r.Name()).String(), effective.AttrString("tsconfig")); err != nil {
					log.Fatal(err)
				}
			}
			if imps.program != nil {
				selected := *imps.program
				selected.Roots = sources
				s.selectProgram(args.Config, &selected)
				imps.program = &selected
			}
			imps.edges = sourceEdges(imps.program.edgesBySource(), sources)
			imps.candidates = imps.program.ownedCandidates(sources)
			// Gazelle's index already holds the provisional sources. Derive its
			// eligible claims once from the refreshed, keep-merged sources.
			imports := selectedRuleImports(args.Config, s.index, effective, args.Rel)
			generated.SetPrivateAttr("_ts_selected_imports", imports)
		}
	}
	if file := s.emission.files[args.Rel]; file != nil && file.File != nil {
		file.Sync()
		s.ruleListsMayChange()
	}
}

// The merger drops a generated rule whose name a rule of another kind holds
// without a word, so the run names the rule to rename.
func reportTakenNames(args language.GenerateArgs, gen []*rule.Rule) {
	if args.File == nil {
		return
	}
	for _, want := range gen {
		for _, have := range args.File.Rules {
			if have.Name() != want.Name() || canonicalRule(args.Config, have).Kind() == want.Kind() {
				continue
			}
			log.Printf("typescript: %s: %s(%s) holds the name Gazelle writes "+
				"for the %s of %s, so the %s is not written; rename the %s",
				args.File.Path, have.Kind(), have.Name(), want.Kind(),
				orRepoRoot(args.Rel), want.Kind(), have.Kind())
		}
	}
}

// packageSrcs is the given repository paths as the package's srcs: relative to
// it, once each, sorted, and every name a label can spell.
func packageSrcs(args language.GenerateArgs, lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, f := range list {
			rel := f
			if args.Rel != "" {
				rel = strings.TrimPrefix(f, args.Rel+"/")
			}
			if seen[rel] {
				continue
			}
			seen[rel] = true
			lbl, ok := srcLabel(rel)
			if !ok {
				reportUnlabelableFile(args, rel)
				continue
			}
			out = append(out, lbl)
		}
	}
	sort.Strings(out)
	return out
}

// A file tsgo may list is a src only when it does; every other regular file
// under the package is one by the walk.
var programExtensions = append([]string{".js", ".jsx", ".mjs", ".cjs"},
	tsSourceExtensions...)

func programCandidate(name string) bool {
	return slices.Contains(programExtensions, strings.ToLower(path.Ext(name)))
}

// dataFiles is every regular file under pkg's tree no listing decides; not a
// deeper package's, a ts_codegen's, a BUILD file or the ts_config's own src.
func (s *programStore) dataFiles(pkg string, tc *tsConfig) []string {
	var out []string
	for _, dir := range slices.Sorted(maps.Keys(s.files)) {
		if !s.walked[dir] || s.foreign[dir] != "" {
			continue
		}
		if dir != pkg && !dirIsAncestorOf(pkg, dir) {
			continue
		}
		if s.nearestPackage(dir) != pkg {
			continue
		}
		for _, f := range s.files[dir] {
			switch {
			case f == "BUILD.bazel", f == "BUILD", f == "package.json", programCandidate(f):
			case path.Join(dir, f) == s.configPath(pkg), dir == pkg && f == "tsconfig.json":
			case codegenWrites(path.Join(dir, f), pkg, tc):
			default:
				out = append(out, path.Join(dir, f))
			}
		}
	}
	return out
}

// nearestPackage is the package at or above dir; "" when there is none.
func (s *programStore) nearestPackage(dir string) string {
	for ; ; dir = parentDir(dir) {
		if s.packages[dir] != nil || dir == "" {
			return dir
		}
	}
}

func codegenLabels(args language.GenerateArgs, s *programStore) []string {
	if args.File == nil {
		return nil
	}
	var out []string
	for _, r := range args.File.Rules {
		r = s.semanticRule(emissionLabel(args.Config.RepoName, args.Rel, ":"+r.Name()))
		if r.Kind() == "ts_codegen" {
			out = append(out, ":"+r.Name())
		}
	}
	return out
}

// tsConfigRule names the directory's tsconfig.json, with the ts_config of every
// tsconfig.json its extends names as a dep, its jsx preserve and its module.
func tsConfigRule(args language.GenerateArgs, tc *tsConfig) *rule.Rule {
	r := rule.NewRule("ts_config", tsConfigTargetName)
	r.SetAttr("src", strings.TrimPrefix(tc.programs.configPath(args.Rel), args.Rel+"/"))
	var deps []string
	for _, base := range tc.programs.bases[args.Rel] {
		deps = append(deps, "//"+base+":"+tsConfigTargetName)
	}
	if len(deps) > 0 {
		sort.Strings(deps)
		r.SetAttr("deps", deps)
	}
	if resolved, _ := tc.programs.resolveCompilerConfig(args.Config, tc.programs.configPath(args.Rel), nil); resolved != nil {
		if strings.EqualFold(resolved.Jsx, "preserve") {
			r.SetAttr("jsx", "preserve")
		}
		if !tsconfig.OxcEmits(resolved.Module) && resolved.Module != "" {
			r.SetAttr("module", strings.ToLower(resolved.Module))
		}
	}
	r.SetAttr("visibility", []string{"//visibility:public"})
	return r
}

func existingRule(args language.GenerateArgs, kind, name string) *rule.Rule {
	if args.File == nil {
		return nil
	}
	for _, r := range args.File.Rules {
		if canonicalRule(args.Config, r).Kind() == kind && r.Name() == name {
			return r
		}
	}
	return nil
}

func canonicalRule(c *config.Config, r *rule.Rule) *rule.Rule {
	if wrapped, ok := c.AliasMap[r.Kind()]; ok {
		canonical := *r
		canonical.SetKind(wrapped)
		r = &canonical
	}
	if len(c.KindMap) == 0 {
		return r
	}
	kinds := (&tsLang{}).Kinds()
	if _, known := kinds[r.Kind()]; known {
		return r
	}
	for _, original := range slices.Sorted(maps.Keys(kinds)) {
		kind := original
		seen := map[string]bool{}
		for !seen[kind] {
			seen[kind] = true
			mapped, ok := c.KindMap[kind]
			if !ok {
				break
			}
			kind = mapped.KindName
			if kind == r.Kind() {
				canonical := *r
				canonical.SetKind(original)
				return &canonical
			}
		}
	}
	return r
}

func dirIsAncestorOf(ancestor, descendant string) bool {
	if ancestor == descendant {
		return false
	}
	return ancestor == "" || strings.HasPrefix(descendant, ancestor+"/")
}

func testTargetName(libName string) string {
	return libName + "_test"
}

func codegenWrites(f, pkg string, tc *tsConfig) bool {
	s := tc.programs
	return s.generatedProgramFile(s.inputs[pkg].config, s.index, f, true)
}

// codegenOutDirResult withdraws what Gazelle generates inside an out_dir. A BUILD
// file left there can be emptied but not deleted, so its package outlives the run.
func codegenOutDirResult(args language.GenerateArgs, root string) language.GenerateResult {
	res := emptyResult(args)
	if args.File == nil {
		return res
	}
	log.Printf("typescript: %s is inside %s, a generated output root, so everything in it is "+
		"that target's output and nothing here is a source. Gazelle withdraws the targets it "+
		"generated here and cannot delete the BUILD file, which keeps the directory a package "+
		"of its own; delete it by hand.", args.Rel, root)
	return res
}

// vitestConfigNames are the file names vitest itself looks for, in its own
// order of preference.
var vitestConfigNames = []string{
	"vitest.config.ts", "vitest.config.mts", "vitest.config.cts",
	"vitest.config.js", "vitest.config.mjs", "vitest.config.cjs",
	"vite.config.ts", "vite.config.mts", "vite.config.cts",
	"vite.config.js", "vite.config.mjs", "vite.config.cjs",
}

func (s *programStore) vitestConfigIn(c *config.Config, rel string) string {
	for _, name := range vitestConfigNames {
		switch s.metadataIdentity(c, path.Join(rel, name)) {
		case authoredInput, generatedInput:
			return name
		case unknownInput:
			log.Fatal(s.incompleteOutput(path.Join(rel, name)))
		}
	}
	return ""
}

// vitestConfigTargetName is the filegroup Gazelle writes beside a vitest config
// for the tests in the packages below it.
const vitestConfigTargetName = "vitest_config"

func (s *programStore) hasPackageJSON(c *config.Config, dir string) bool {
	rel, err := filepath.Rel(c.RepoRoot, dir)
	return err == nil && s.metadataIdentity(c, path.Join(filepath.ToSlash(rel), "package.json")) != unavailableInput
}

// vitestRootAbove is where plain `vitest` reads its config for a test in dir with
// none of its own: the nearest ancestor holding a package.json, else repoRoot.
func vitestRootAbove(c *config.Config, s *programStore, dir string) string {
	repoRoot := c.RepoRoot
	if dir == repoRoot || s.hasPackageJSON(c, dir) {
		return ""
	}
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
		if dir == repoRoot || s.hasPackageJSON(c, dir) {
			return dir
		}
	}
}

// exportedVitestConfig is the config the directory at rel makes a label for
// the packages below: beside a package.json or at the root, as vitest reads it.
func exportedVitestConfig(c *config.Config, s *programStore, dir, rel string) string {
	if rel != "" && !s.hasPackageJSON(c, dir) {
		return ""
	}
	name := s.vitestConfigIn(c, rel)
	if name == "" || packageName(rel) == vitestConfigTargetName {
		return ""
	}
	return name
}

// vitestConfigRule is the filegroup over the directory's exported config.
func vitestConfigRule(args language.GenerateArgs) *rule.Rule {
	r := rule.NewRule("filegroup", vitestConfigTargetName)
	name := exportedVitestConfig(args.Config, getConfig(args.Config).programs, args.Dir, args.Rel)
	r.SetAttr("srcs", srcLabels([]string{name}))
	r.SetAttr("visibility", []string{"//visibility:public"})
	return r
}

// configFilegroup is the filegroup of that name an exported vitest config puts
// in its directory: over the config, or over the wrangler config it names.
func configFilegroup(args language.GenerateArgs, tc *tsConfig, cfgName,
	name string) *rule.Rule {
	if name == vitestConfigTargetName {
		return vitestConfigRule(args)
	}
	return wranglerConfigRule(args, tc, path.Join(args.Rel, cfgName))
}

func vitestConfigFor(args language.GenerateArgs, tc *tsConfig) (attr, cfg string) {
	name := testTargetName(packageName(args.Rel))
	stored := existingRule(args, "ts_test", name)
	if stored != nil && (stored.ShouldKeep() || attrKept(stored, "config")) {
		return selectedVitestConfig(args, tc, stored)
	}
	proposed := rule.NewRule("ts_test", name)
	if attr := defaultVitestConfigFor(args, tc); attr != "" {
		proposed.SetAttr("config", attr)
	}
	selected := rule.NewRule("ts_test", name)
	if stored != nil && stored.Attr("config") != nil {
		selected.SetAttr("config", stored.Attr("config"))
	}
	rule.MergeRules(proposed, selected, map[string]bool{"config": true}, args.Dir)
	return selectedVitestConfig(args, tc, selected)
}

func configIsAbsent(expr bzl.Expr) bool {
	value, ok := expr.(*bzl.Ident)
	return expr == nil || ok && value.Name == "None"
}

func selectedVitestConfig(args language.GenerateArgs, tc *tsConfig, selectedRule *rule.Rule) (attr, cfg string) {
	unknown := func(reason string) (string, string) {
		if !selectedRule.ShouldKeep() {
			log.Fatalf("typescript: %s cannot establish its selected config root: %s; refusing to regenerate config runtime inputs. Did you mean to use a literal config label supplying one known file, set config = None, or keep the whole ts_test rule and maintain its runtime inputs manually?", label.New(args.Config.RepoName, args.Rel, selectedRule.Name()), reason)
		}
		return attr, ""
	}
	expr := selectedRule.Attr("config")
	if configIsAbsent(expr) {
		return "", ""
	}
	value, literal := expr.(*bzl.StringExpr)
	if !literal {
		return unknown("config is an expression")
	}
	attr = value.Value
	if attr == "" {
		return unknown("config is an empty label")
	}
	selected, pkg := attr, args.Rel
	seen := map[label.Label]bool{}
	for selected != "" {
		lbl, err := parseLabel(selected)
		if err != nil {
			return unknown("invalid config label " + selected)
		}
		lbl = sourceLabelIdentity(lbl, language.GenerateArgs{Config: args.Config, Rel: pkg})
		if lbl.Canonical || lbl.Repo != args.Config.RepoName || seen[lbl] {
			return unknown("external or cyclic config label " + lbl.String())
		}
		seen[lbl] = true
		var target *rule.Rule
		if g := tc.programs.emission; g != nil {
			key := emissionLabel(args.Config.RepoName, lbl.Pkg, ":"+lbl.Name)
			target = tc.programs.semanticRule(key)
			if target == nil {
				tc.programs.observeBuild(lbl.Pkg, walk.GetDirInfo)
				target = tc.programs.semanticRule(key)
			}
		}
		if target != nil && target.Kind() == "filegroup" {
			if group := target.Attr("output_group"); group != nil {
				if value, literal := group.(*bzl.StringExpr); !literal || value.Value != "" {
					return unknown(lbl.String() + " selects an output_group")
				}
			}
		}
		if lbl.Name == vitestConfigTargetName && (lbl.Pkg == "" || tc.programs.packages[lbl.Pkg] != nil) &&
			(target == nil || target.Kind() == "filegroup" && !target.ShouldKeep() && !attrKept(target, "srcs")) {
			if name := exportedVitestConfig(args.Config, tc.programs, filepath.Join(args.Config.RepoRoot, filepath.FromSlash(lbl.Pkg)), lbl.Pkg); name != "" {
				return attr, path.Join(lbl.Pkg, name)
			}
		}
		if target == nil {
			return attr, path.Join(lbl.Pkg, lbl.Name)
		}
		pkg = lbl.Pkg
		if target.Kind() == "alias" {
			actual, incomplete := runtimeLabels(target, "actual", false)
			if incomplete != "" || len(actual) != 1 || actual[0] == "" {
				return unknown(lbl.String() + " has no known actual label")
			}
			selected = actual[0]
			continue
		}
		outputs := ruleOutputs(target, args.Config.RepoName, pkg)
		files := srcLabels(outputs.files)
		if target.Kind() == "filegroup" {
			var incomplete string
			files, incomplete = runtimeLabels(target, "srcs", true)
			if incomplete != "" {
				return unknown(lbl.String() + " has expression-valued srcs")
			}
		} else if outputs.incomplete != "" || outputs.tree != "" {
			return unknown(lbl.String() + " has unknown output membership")
		}
		if len(files) != 1 {
			return unknown(lbl.String() + " does not supply exactly one config file")
		}
		selected = files[0]
	}
	return unknown("config forwarding ends in an empty label")
}

func defaultVitestConfigFor(args language.GenerateArgs, tc *tsConfig) string {
	if name := tc.programs.vitestConfigIn(args.Config, args.Rel); name != "" {
		return name
	}
	repoRoot := args.Config.RepoRoot
	dir := vitestRootAbove(args.Config, tc.programs, args.Dir)
	if dir == "" {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, dir)
	if err != nil {
		return ""
	}
	if rel = filepath.ToSlash(rel); rel == "." {
		rel = ""
	}
	name := exportedVitestConfig(args.Config, tc.programs, dir, rel)
	if name == "" {
		return ""
	}
	if rel != "" && tc.programs.packages[rel] == nil {
		log.Printf("typescript: %s: plain vitest reads %s for the tests here, "+
			"but %s is not a package, so no label reaches it and the test "+
			"runs with no config", args.Rel, path.Join(rel, name), rel)
		return ""
	}
	return "//" + rel + ":" + vitestConfigTargetName
}

// ruleImports is what GenerateRules hands Resolve for one rule: the edges of
// the files it compiles, its vitest config and the deps no edge names.
type ruleImports struct {
	program        *program
	candidates     []resolutionCandidate
	edges          []explainfiles.Edge
	config         string
	deps           []string
	reportSrcDrops func(*rule.Rule)
}

// The index is shared by every reader and must not be written.
func (p *program) edgesBySource() map[string][]explainfiles.Edge {
	if p == nil {
		return map[string][]explainfiles.Edge{}
	}
	if index := p.bySource; index != nil && len(index.edges) == len(p.Edges) && (len(p.Edges) == 0 || &index.edges[0] == &p.Edges[0]) {
		return index.byFrom
	}
	byFrom := map[string][]explainfiles.Edge{}
	for _, e := range p.Edges {
		byFrom[e.From] = append(byFrom[e.From], e)
	}
	p.bySource = &edgeIndex{edges: p.Edges, byFrom: byFrom}
	return byFrom
}

type edgeIndex struct {
	edges  []explainfiles.Edge
	byFrom map[string][]explainfiles.Edge
}

func sourceEdges(byFrom map[string][]explainfiles.Edge, files ...[]string) []explainfiles.Edge {
	from := map[string]bool{}
	for _, list := range files {
		for _, f := range list {
			from[f] = true
		}
	}
	var out []explainfiles.Edge
	for _, f := range slices.Sorted(maps.Keys(from)) {
		out = append(out, byFrom[f]...)
	}
	return out
}

// A ts_test's runtime is its deps, so the manifest union joins the edges.
func (s *programStore) testImports(lock *npmLock,
	pkg, compile, cfg string) *ruleImports {
	imps := &ruleImports{config: cfg}
	if compile != "" {
		imps.deps = append(imps.deps, compile)
	}
	if lock != nil {
		m := s.nearestManifest(s.repoConfig, pkg)
		imps.deps = append(imps.deps, lock.manifestLabels(m, pkg)...)
	}
	return imps
}

func (s *programStore) appPackage(c *config.Config, rel string, sources []string) bool {
	if s.metadataIdentity(c, path.Join(rel, "index.html")) == authoredInput {
		return true
	}
	for _, source := range sources {
		switch strings.ToLower(path.Base(source)) {
		case "main.ts", "main.tsx", "app.ts", "app.tsx":
			return true
		}
	}
	return false
}

func packageScopeSources(args language.GenerateArgs, tc *tsConfig, res language.GenerateResult) []string {
	s := tc.programs
	owner := func(file string) string {
		boundary := ""
		if (len(res.Gen) > 0 || len(args.OtherGen) > 0) && within(file, args.Rel) {
			boundary = args.Rel
		}
		return configSrcPackage(tc, file, boundary)
	}
	if s.foreign[args.Rel] != "" || owner(path.Join(args.Rel, "package.json")) != args.Rel {
		return nil
	}
	scopes := map[string]bool{}
	// Programs share most files, and a directory's files share its scope.
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, programs := range []map[string]*program{s.programs, s.vitestPrograms} {
		for _, p := range programs {
			for _, file := range p.Files {
				if files[file] {
					continue
				}
				files[file] = true
				if !firstParty(file) || !programCandidate(file) || s.generatedProgramFile(args.Config, s.index, file, true) || dirs[parentDir(file)] {
					continue
				}
				dirs[parentDir(file)] = true
				scope := s.packageScope(args.Config, parentDir(file))
				if scope != "" && s.inputIdentity(args.Config, s.index, scope) == authoredInput && owner(scope) == args.Rel {
					scopes[scope] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(scopes))
}

func unclaimedPackageScopes(args language.GenerateArgs, tc *tsConfig, res language.GenerateResult) []string {
	s := tc.programs
	scopes := packageScopeSources(args, tc, res)
	if len(scopes) == 0 {
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(s.emission.rules)) {
		r := s.semanticRule(key)
		owner, err := parseLabel(key)
		if err != nil || (r.Kind() != "ts_compile" && r.Kind() != "ts_test") {
			continue
		}
		if owner.Pkg == args.Rel && r.Kind() == "ts_compile" && r.Name() == packageName(args.Rel) {
			if r.ShouldKeep() || attrKept(r, "srcs") || attrKept(r, "package_scopes") || attrKept(r, "data") {
				return nil
			}
			continue
		}
		inputs := rule.NewRule("ts_compile", r.Name())
		file := s.emission.files[owner.Pkg]
		if file == nil {
			file = &rule.File{Pkg: owner.Pkg}
		}
		for _, attr := range []string{"srcs", "package_scopes", "data"} {
			if attr == "package_scopes" && owner.Pkg != args.Rel {
				continue
			}
			if r.Kind() == "ts_test" && attr != "srcs" {
				continue
			}
			if r.Attr(attr) == nil {
				continue
			}
			expr := r.Attr(attr)
			if r.Kind() == "ts_test" && owner.Pkg == args.Rel && managedProgramRule(r, args.Rel) {
				expr = keptSourceRule(r, file).Attr("srcs")
			}
			if expr == nil {
				inputs.DelAttr("srcs")
			} else {
				inputs.SetAttr("srcs", expr)
			}
			claimed := declaredSourceImports(s.inputs[owner.Pkg].config, inputs, file)
			if claimed.incomplete != "" && owner.Pkg == args.Rel {
				return nil
			}
			for _, spec := range claimed.imports {
				scopes = slices.DeleteFunc(scopes, func(scope string) bool { return scope == spec.Imp })
			}
		}
	}
	return scopes
}

func exportVisibility(c *config.Config, r *rule.Rule, pkg string) ([]string, error) {
	if r.Attr("visibility") == nil {
		return []string{"//visibility:public"}, nil
	}
	return literalVisibility(c, r.Attr("visibility"), pkg, "exports_files visibility")
}

func literalVisibility(c *config.Config, expr bzl.Expr, pkg, description string) ([]string, error) {
	list, literal := expr.(*bzl.ListExpr)
	if !literal {
		return nil, fmt.Errorf("%s is an expression", description)
	}
	if len(list.List) == 0 {
		return []string{"//visibility:private"}, nil
	}
	var values []string
	for _, item := range list.List {
		value, literal := item.(*bzl.StringExpr)
		if !literal {
			return nil, fmt.Errorf("%s is an expression", description)
		}
		if _, err := parseLabel(value.Value); err != nil {
			return nil, fmt.Errorf("%s %q: %w", description, value.Value, err)
		}
		values = append(values, emissionLabel(c.RepoName, pkg, value.Value))
	}
	slices.Sort(values)
	return slices.Compact(values), nil
}

func exportSourceMembership(r *rule.Rule) (*bzl.ListExpr, error) {
	expr := r.Attr("srcs")
	if len(r.Args()) == 1 && expr == nil {
		expr = r.Args()[0]
	} else if len(r.Args()) != 0 {
		return nil, fmt.Errorf("exports_files source membership is an expression")
	}
	list, literal := expr.(*bzl.ListExpr)
	if !literal {
		return nil, fmt.Errorf("exports_files source membership is an expression")
	}
	for _, item := range list.List {
		if _, literal := item.(*bzl.StringExpr); !literal {
			return nil, fmt.Errorf("exports_files source membership is an expression")
		}
	}
	return list, nil
}

func (s *programStore) sourceVisibility(c *config.Config, f *rule.File, source string) ([]string, bool, error) {
	var visibility []string
	if f != nil {
		for _, r := range f.Rules {
			if r.Kind() != "exports_files" || r.PrivateAttr("_ts_source_export") == "pending" {
				continue
			}
			list, err := exportSourceMembership(r)
			if err != nil {
				return nil, false, err
			}
			contains := false
			for _, item := range list.List {
				value := item.(*bzl.StringExpr)
				contains = contains || path.Clean(value.Value) == path.Clean(source)
			}
			if !contains {
				continue
			}
			policy, err := exportVisibility(c, r, f.Pkg)
			if err != nil {
				return nil, true, err
			}
			if visibility != nil && !slices.Equal(visibility, policy) {
				return nil, true, fmt.Errorf("exports_files declarations give %s different visibility", source)
			}
			visibility = policy
		}
	}
	if visibility != nil {
		return visibility, true, nil
	}
	if f != nil && f.Content == nil {
		if original := s.sourceExportAncestor(f.Pkg); original != nil {
			name := strings.TrimPrefix(path.Join(f.Pkg, source), original.Pkg+"/")
			policy, exported, err := s.sourceVisibility(c, original, name)
			if exported || err != nil {
				return policy, false, err
			}
		}
	}
	if f != nil {
		for _, r := range f.Rules {
			if r.Kind() == "package" && r.Attr("default_visibility") != nil {
				visibility, err := literalVisibility(c, r.Attr("default_visibility"), f.Pkg, "package default_visibility")
				return visibility, false, err
			}
		}
	}
	return []string{"//:__subpackages__"}, false, nil
}

func withPackageScopeCompiler(args language.GenerateArgs, tc *tsConfig, res language.GenerateResult) language.GenerateResult {
	name := packageName(args.Rel)
	for _, generated := range res.Gen {
		r := canonicalRule(args.Config, generated)
		if r.Kind() == "ts_compile" && r.Name() == name {
			return res
		}
	}
	if len(unclaimedPackageScopes(args, tc, res)) == 0 {
		return res
	}
	r := rule.NewRule("ts_compile", name)
	r.SetAttr("srcs", []string{})
	r.SetAttr("package_scopes", []string{})
	r.SetPrivateAttr("_ts_scope_candidate", true)
	r.SetAttr("visibility", []string{"//visibility:private"})
	res.Gen = append(res.Gen, r)
	res.Imports = append(res.Imports, nil)
	res.Empty = slices.DeleteFunc(res.Empty, func(r *rule.Rule) bool { return r.Kind() == "ts_compile" && r.Name() == name })
	return res
}

func withSourceExports(args language.GenerateArgs, tc *tsConfig, res language.GenerateResult) language.GenerateResult {
	if args.File != nil || args.Rel != "" && len(res.Gen)+len(args.OtherGen) == 0 {
		return res
	}
	original := tc.programs.sourceExportAncestor(args.Rel)
	var declarations []*rule.Rule
	if original != nil {
		declarations = original.Rules
	}
	for _, old := range declarations {
		if old.Kind() != "exports_files" {
			continue
		}
		list, err := exportSourceMembership(old)
		if err != nil {
			continue
		}
		var values []bzl.Expr
		for _, item := range list.List {
			source := path.Join(original.Pkg, item.(*bzl.StringExpr).Value)
			if within(source, args.Rel) {
				values = append(values, &bzl.StringExpr{Value: strings.TrimPrefix(source, args.Rel+"/")})
			}
		}
		if len(values) == 0 {
			continue
		}
		target := rule.NewRule("exports_files", "")
		target.SetAttr("srcs", &bzl.ListExpr{List: values})
		target.SetPrivateAttr("_ts_source_export", "relocated")
		if visibility := old.Attr("visibility"); visibility != nil {
			target.SetAttr("visibility", visibility)
			if labels, err := exportVisibility(args.Config, old, original.Pkg); err == nil {
				target.SetAttr("visibility", labels)
			}
		}
		if licenses := old.Attr("licenses"); licenses != nil {
			target.SetAttr("licenses", licenses)
		}
		res.Gen = append(res.Gen, target)
		res.Imports = append(res.Imports, nil)
	}
	var scopes []string
	for dir := range tc.programs.files {
		source := path.Join(dir, "package.json")
		if !within(dir, args.Rel) || !tc.programs.sourceFile(source) ||
			tc.programs.inputIdentity(args.Config, tc.programs.index, source) != authoredInput {
			continue
		}
		scopes = append(scopes, source)
	}
	slices.Sort(scopes)
	for _, source := range scopes {
		target := rule.NewRule("exports_files", "")
		target.SetAttr("srcs", []string{strings.TrimPrefix(source, args.Rel+"/")})
		target.SetPrivateAttr("_ts_source_export", "pending")
		res.Gen = append(res.Gen, target)
		res.Imports = append(res.Imports, nil)
	}
	return res
}

func (s *programStore) relocateSourceExports(c *config.Config, pkg string) {
	file := s.emission.files[pkg]
	if file == nil || file.Content != nil || !s.generatedPackages[pkg] || pkg == "" {
		return
	}
	boundary := s.bazelPackage(pkg)
	for _, target := range file.Rules {
		if target.PrivateAttr("_ts_source_export") != "relocated" {
			continue
		}
		list, err := exportSourceMembership(target)
		if err != nil {
			log.Fatal(err)
		}
		list.List = slices.DeleteFunc(list.List, func(item bzl.Expr) bool {
			return !boundary || sourceExportOwner(s, path.Join(pkg, item.(*bzl.StringExpr).Value)) != pkg
		})
		if len(list.List) == 0 {
			target.Delete()
		} else {
			target.SetAttr("srcs", list)
		}
	}
	if !boundary {
		return
	}
	original := s.sourceExportAncestor(pkg)
	if original == nil {
		return
	}
	ancestor := original.Pkg
	for _, old := range original.Rules {
		if old.Kind() != "exports_files" {
			continue
		}
		list, err := exportSourceMembership(old)
		if err != nil {
			log.Fatalf("typescript: %s: %v; use literal source lists before creating package %s", s.emission.files[ancestor].Path, err, pkg)
		}
		var kept []bzl.Expr
		var moved []string
		keptMembership := old.ShouldKeep() || attrKept(old, "srcs") || rule.ShouldKeep(list)
		for _, item := range list.List {
			text := item.(*bzl.StringExpr)
			source := path.Join(ancestor, text.Value)
			if sourceExportOwner(s, source) != pkg {
				kept = append(kept, item)
				continue
			}
			moved = append(moved, strings.TrimPrefix(source, pkg+"/"))
			keptMembership = keptMembership || rule.ShouldKeep(item)
		}
		if len(moved) == 0 {
			continue
		}
		if keptMembership {
			log.Fatalf("typescript: %s: kept exports_files prevents creating package %s; remove # keep to relocate the exports to their canonical package", s.emission.files[ancestor].Path, pkg)
		}
		_, err = exportVisibility(c, old, ancestor)
		if err != nil {
			log.Fatalf("typescript: %s: %v; use explicit visibility labels before creating package %s", s.emission.files[ancestor].Path, err, pkg)
		}
		licenses := old.AttrStrings("licenses")
		licensesList, licensesLiteral := old.Attr("licenses").(*bzl.ListExpr)
		if old.Attr("licenses") != nil && (!licensesLiteral || len(licensesList.List) != len(licenses)) {
			log.Fatalf("typescript: %s: exports_files licenses is an expression; use explicit licenses before creating package %s", s.emission.files[ancestor].Path, pkg)
		}
		if !s.generatedPackages[ancestor] {
			log.Fatalf("typescript: moving source exports from %s to %s requires both BUILD files in this update. Did you mean to include %s and enable TypeScript generation there?", orRepoRoot(ancestor), pkg, orRepoRoot(ancestor))
		}
		list.List = kept
		if len(kept) == 0 {
			old.Delete()
		} else if old.Attr("srcs") != nil {
			old.SetAttr("srcs", list)
		} else if err := old.UpdateArg(0, list); err != nil {
			log.Fatal(err)
		}
	}
}

func sourceExportOwner(s *programStore, source string) string {
	for dir := parentDir(source); ; dir = parentDir(dir) {
		if s.bazelPackage(dir) {
			return dir
		}
	}
}

func (s *programStore) sourceExportAncestor(pkg string) *rule.File {
	if s.emission == nil || pkg == "" {
		return nil
	}
	for dir := parentDir(pkg); ; dir = parentDir(dir) {
		if file := s.emission.files[dir]; file != nil && file.Content != nil {
			return file
		}
		if dir == "" {
			return nil
		}
	}
}

func (s *programStore) bazelPackage(dir string) bool {
	if dir == "" {
		return true
	}
	file := s.emission.files[dir]
	if file == nil {
		return false
	}
	if file.Content != nil {
		return true
	}
	return slices.ContainsFunc(file.Rules, func(r *rule.Rule) bool {
		return r.PrivateAttr("_ts_source_export") == nil &&
			(r.PrivateAttr("_ts_scope_candidate") != true || len(r.AttrStrings("package_scopes")) > 0)
	})
}

func (p *program) ownedCandidates(files ...[]string) []resolutionCandidate {
	if p == nil {
		return nil
	}
	from := map[string]bool{}
	for _, list := range files {
		for _, file := range list {
			from[file] = true
		}
	}
	var out []resolutionCandidate
	for _, candidate := range p.candidates {
		if from[candidate.from] {
			out = append(out, candidate)
		}
	}
	return out
}
