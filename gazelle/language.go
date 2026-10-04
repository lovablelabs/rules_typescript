// Package typescript is the Gazelle language for rules_typescript: a package
// per tsconfig.json program, its targets from tsgo's listing.
package typescript

import (
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	"github.com/bazelbuild/rules_go/go/runfiles"
)

const languageName = "typescript"

type tsLang struct {
	language.BaseLifecycleManager
	programs *programStore
}

func NewLanguage() language.Language {
	return &tsLang{}
}

func (l *tsLang) Name() string { return languageName }

func (l *tsLang) RegisterFlags(fs *flag.FlagSet, _ string, c *config.Config) {
	tc := defaultTsConfig()
	fs.StringVar(&tc.programs.tsgoFlag, "ts_tsgo", "",
		"the tsgo binary that lists each tsconfig.json program; the default is the toolchain's, in the Gazelle binary's runfiles")
	fs.BoolVar(&tc.programs.verbose, "ts_verbose", false,
		"say which tsgo lists the programs, one line per tsconfig.json with "+
			"what it listed or why it is not a package, how many are, and the "+
			".ts files no program lists")
	fs.StringVar(&tc.protos.graphPath, "ts_proto_graph", "", "declared native external protobuf graph artifact")
	l.programs = tc.programs
	c.Exts[languageName] = tc
}

func (l *tsLang) CheckFlags(fs *flag.FlagSet, c *config.Config) error {
	store := getConfig(c).protos
	if store.graphPath != "" {
		graphFile, err := runfiles.Rlocation(store.graphPath)
		if err != nil {
			return err
		}
		graph, err := loadProtoGraph(graphFile)
		if err != nil {
			return err
		}
		store.graph = graph
	}
	dirs := fs.Args()
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	store.roots = nil
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(c.WorkDir, dir)
		}
		rel, err := filepath.Rel(c.RepoRoot, filepath.Clean(dir))
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		store.roots = append(store.roots, filepath.ToSlash(rel))
	}
	if recursive := fs.Lookup("r"); recursive != nil {
		store.recursive = recursive.Value.String() == "true"
	}
	getConfig(c).programs.fullWalk = store.recursive && slices.Equal(store.roots, []string{""})

	if tsgo := getConfig(c).programs.tsgoFlag; tsgo != "" {
		if _, err := os.Stat(tsgo); err != nil {
			return fmt.Errorf("-ts_tsgo: %w", err)
		}
	}
	return nil
}

func (l *tsLang) KnownDirectives() []string {
	return []string{"ts_proto"}
}

func (l *tsLang) Configure(c *config.Config, rel string, f *rule.File) {
	getConfig(c).programs.ruleListsMayChange()
	configureTsConfig(c, rel, f, walk.GetDirInfo)
	l.programs = getConfig(c).programs
	l.programs.observeBuild(rel, walk.GetDirInfo)
	if len(c.Langs) > 0 && !slices.Contains(c.Langs, languageName) {
		l.programs.walked[rel] = false
	}
	info, err := walk.GetDirInfo(rel)
	if err != nil {
		log.Fatalf("typescript: %s: %v", rel, err)
	}
	l.programs.visit(rel, info.RegularFiles)
	for _, base := range l.programs.bases[rel] {
		l.observeSources([]string{tsconfigIn(base)})
	}
	if p := l.programs.programs[rel]; p != nil {
		p = l.programs.withKeptSources(c, p, rel, walk.GetDirInfo)
		l.programs.record(p)
		l.observeProgram(p)
	}
}

func (l *tsLang) observeProgram(p *program) {
	files := slices.Clone(p.Files)
	for _, candidate := range p.candidates {
		files = append(files, candidate.path)
	}
	l.observeSources(files)
}

// Resolve runs after the native walk releases GetDirInfo's BUILD and exclusion facts.
func (l *tsLang) observeSources(files []string) {
	observed := map[string]bool{}
	var dirs []string
	for _, file := range files {
		dir := parentDir(file)
		if !firstParty(file) || observed[dir] {
			continue
		}
		observed[dir] = true
		dirs = append(dirs, dir)
	}
	l.programs.observeBuilds(dirs, walk.GetDirInfo)
}

func (l *tsLang) Kinds() map[string]rule.KindInfo {
	return map[string]rule.KindInfo{
		"exports_files": {
			MatchAttrs: []string{"srcs"},
		},
		"ts_compile": {
			NonEmptyAttrs: map[string]bool{
				"srcs":           true,
				"package_scopes": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs":                true,
				"type_inputs":         true,
				"package_scopes":      true,
				"deps":                true,
				"visibility":          true,
				"tsconfig":            true,
				"node_modules":        true,
				"source_node_modules": true,
				"emit":                true,
			},
			ResolveAttrs: map[string]bool{
				"source_node_modules": true,
				"srcs":                true,
				"type_inputs":         true,
				"package_scopes":      true,
				"deps":                true,
			},
		},
		"ts_test": {
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs":                true,
				"type_inputs":         true,
				"package_scopes":      true,
				"deps":                true,
				"tsconfig":            true,
				"config":              true,
				"node_modules":        true,
				"source_node_modules": true,
				"emit":                true,
			},
			ResolveAttrs: map[string]bool{
				"source_node_modules": true,
				"srcs":                true,
				"type_inputs":         true,
				"package_scopes":      true,
				"test_srcs":           true,
				"deps":                true,
				"config_srcs":         true,
				"config_node_modules": true,
				"workers_pool":        true,
				"wrangler_config":     true,
				"coverage_provider":   true,
			},
		},
		// ts_config makes a package's tsconfig.json a label. deps, jsx and module
		// are read out of the file, so all three are Gazelle's (# keep otherwise).
		"ts_config": {
			NonEmptyAttrs: map[string]bool{
				"src": true,
			},
			MergeableAttrs: map[string]bool{
				"src":        true,
				"deps":       true,
				"jsx":        true,
				"module":     true,
				"visibility": true,
			},
		},
		// ts_codegen is hand-written and never generated; a Kind so that its
		// out_dir and scalar outputs participate in dependency resolution.
		"ts_codegen":      {},
		"ts_proto_config": {},
		"ts_proto_library": {
			NonEmptyAttrs:  map[string]bool{"proto": true},
			MergeableAttrs: map[string]bool{"proto": true, "out_dir": true, "tsconfig": true, "node_modules": true, "source_node_modules": true, "options": true, "deps": true, "type_inputs": true, "visibility": true},
			ResolveAttrs:   map[string]bool{"deps": true, "type_inputs": true, "source_node_modules": true},
		},
		"ts_dev_server": {
			NonEmptyAttrs: map[string]bool{"entry_point": true},
			MergeableAttrs: map[string]bool{
				"entry_point":  true,
				"node_modules": true,
				"plugin":       true,
				"visibility":   true,
			},
		},
		// filegroup makes a vitest config, or the wrangler config it names, a
		// label for the packages below it.
		"filegroup": {
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs":       true,
				"visibility": true,
			},
		},
		// An importer declaring nothing has no deps, so deps, parent and
		// hoist together decide emptiness.
		"node_modules": {
			NonEmptyAttrs: map[string]bool{
				"deps":   true,
				"parent": true,
				"hoist":  true,
			},
			MergeableAttrs: map[string]bool{
				"deps":       true,
				"parent":     true,
				"hoist":      true,
				"visibility": true,
			},
		},
		"node_modules_member": {
			NonEmptyAttrs: map[string]bool{
				"member": true,
			},
			MergeableAttrs: map[string]bool{
				"member":     true,
				"visibility": true,
			},
		},
		// The lockfile package's store call, named after its directory.
		"npm_virtual_store": {},
	}
}

// The load each kind comes from; filegroup is native. The hub is spelled
// @npm, as every npm label Gazelle writes is.
var kindLoads = map[string]string{
	"ts_codegen":          "//ts:defs.bzl",
	"ts_proto_library":    "//proto:defs.bzl",
	"ts_proto_config":     "//proto:defs.bzl",
	"ts_compile":          "//ts:defs.bzl",
	"ts_config":           "//ts:defs.bzl",
	"ts_dev_server":       "//ts:defs.bzl",
	"ts_test":             "//ts:defs.bzl",
	"node_modules":        "//npm:defs.bzl",
	"node_modules_member": "//npm:defs.bzl",
	"npm_virtual_store":   "@npm//:defs.bzl",
}

func (l *tsLang) Loads() []rule.LoadInfo {
	return l.ApparentLoads(func(string) string { return "" })
}

func (l *tsLang) ApparentLoads(
	moduleToApparentName func(string) string,
) []rule.LoadInfo {
	rulesTs := moduleToApparentName("rules_typescript")
	if rulesTs == "" {
		rulesTs = "rules_typescript"
	}
	symbols := map[string][]string{}
	for kind := range l.Kinds() {
		file, ok := kindLoads[kind]
		if !ok {
			continue
		}
		if !strings.HasPrefix(file, "@") {
			file = "@" + rulesTs + file
		}
		symbols[file] = append(symbols[file], kind)
	}
	var loads []rule.LoadInfo
	for _, file := range slices.Sorted(maps.Keys(symbols)) {
		sort.Strings(symbols[file])
		loads = append(loads, rule.LoadInfo{Name: file, Symbols: symbols[file]})
	}
	return loads
}

// Fix is empty: no ruleset code knows a retired attribute.
func (l *tsLang) Fix(_ *config.Config, _ *rule.File) {}

func (l *tsLang) GenerateRules(args language.GenerateArgs) language.GenerateResult {
	getConfig(args.Config).programs.ruleListsMayChange()
	res := generateRules(args)
	tc := getConfig(args.Config)
	s := tc.programs
	producer, _, _ := s.declaredOutputProducer(args.Rel, true)
	if input := s.inputs[args.Rel]; producer == nil && input.program != nil && s.generatedPackages[args.Rel] {
		res = registerProgramRules(args, res)
		input.refresh = func() { refreshProgramRules(args, tc, res) }
		s.inputs[args.Rel] = input
		if file := s.emission.files[args.Rel]; file != nil && file.File == nil {
			file.Rules = append(slices.Clone(args.OtherGen), res.Gen...)
			l.programs.ruleListsMayChange()
		}
	}
	for i, imports := range res.Imports {
		r := res.Gen[i]
		if existing := existingRule(args, r.Kind(), r.Name()); existing != nil &&
			(existing.ShouldKeep() || attrKept(existing, "tsconfig")) {
			r = existing
		}
		selected, owner := r.AttrString("tsconfig"), args.Rel
		switch imps := imports.(type) {
		case *ruleImports:
			if imps.config != "" {
				if p := l.programs.configProgram(args.Config.RepoRoot, imps.config); p != nil {
					l.observeProgram(p)
					consumer := existingRule(args, canonicalRule(args.Config, r).Kind(), r.Name())
					if consumer == nil {
						consumer = r
					}
					if !consumer.ShouldKeep() && !attrKept(consumer, "wrangler_config") && importsWorkersPool(p.Edges) {
						l.programs.observeWranglerConfig(args.Config, imps.config)
					}
				}
			}
		case *protoRuleImports:
			owner = imps.identity.owner
		}
		if p := l.programs.compilerProgram(args.Config, selected, owner, walk.GetDirInfo); p != nil {
			l.observeProgram(p)
		}
	}
	return res
}

func (l *tsLang) DoneGeneratingRules() {
	l.programs.ruleListsMayChange()
	if l.programs != nil {
		l.programs.stopPrefetch()
		l.programs.reportCensus()
		l.programs.reportUnlisted()
		l.programs.reportUnowned()
	}
}

func (l *tsLang) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {
	getConfig(c).programs.ruleListsMayChange()
	// Owner selection runs before every indexed rule has reached Resolve.
	if g := getConfig(c).programs.emission; g != nil {
		if getConfig(c).programs.generatedPackages[f.Pkg] {
			g.setFile(f.Pkg, f)
		}
		g.setRule(emissionLabel(c.RepoName, f.Pkg, ":"+r.Name()), r)
	}
	if l.programs != nil {
		return importsForRule(c, r, f, walk.GetDirInfo)
	}
	return importsForRule(c, r, f)
}

func (l *tsLang) Embeds(_ *rule.Rule, _ label.Label) []label.Label { return nil }

func (l *tsLang) Resolve(
	c *config.Config,
	ix *resolve.RuleIndex,
	_ *repo.RemoteCache,
	r *rule.Rule,
	imports any,
	from label.Label,
) {
	getConfig(c).programs.ruleListsMayChange()
	liveRule := r
	r = canonicalRule(c, r)
	tc := getConfig(c)
	tc.programs.selectInputs(ix)
	effective := r
	if g := tc.programs.emission; g != nil {
		if f := g.files[from.Pkg]; f != nil {
			for _, merged := range f.Rules {
				if canonicalRule(c, merged).Kind() == r.Kind() && merged.Name() == r.Name() {
					effective = canonicalRule(c, merged)
					break
				}
			}
		}
	}
	if info := l.Kinds()[r.Kind()]; len(info.NonEmptyAttrs) > 0 && effective.IsEmpty(info) {
		return
	}
	if effective.PrivateAttr("_ts_scope_candidate") == true {
		return
	}
	check := func(file string) {
		if tc.programs.requireInput(c, ix, file, from.String()) == generatedInput {
			if producer, _ := tc.programs.outputProducer(file); producer == nil {
				log.Fatalf("typescript: %s requires generated file %s, but only a generated tree or compiler owner declares it. Did you mean to declare the file as a scalar output for this attribute?", from, file)
			}
		}
	}
	if !effective.ShouldKeep() {
		switch effective.Kind() {
		case "ts_config":
			if effective.Name() != tsConfigTargetName {
				break
			}
			if !attrKept(effective, "src") {
				check(path.Join(from.Pkg, effective.AttrString("src")))
			}
			if !attrKept(effective, "deps") {
				for _, base := range tc.programs.bases[from.Pkg] {
					check(tsconfigIn(base))
				}
			}
		case "filegroup":
			if (effective.Name() == vitestConfigTargetName || effective.Name() == wranglerConfigTargetName) && !attrKept(effective, "srcs") {
				for _, src := range effective.AttrStrings("srcs") {
					lbl, err := parseLabel(src)
					if err != nil {
						continue
					}
					lbl = sourceLabelIdentity(lbl, language.GenerateArgs{Config: c, Rel: from.Pkg})
					if lbl.Repo == c.RepoName {
						check(path.Join(lbl.Pkg, lbl.Name))
					}
				}
			}
		}
	}
	if imps, ok := imports.(*protoRuleImports); ok && imps != nil {
		resolveProtoLibrary(c, ix, r, effective, imps, from)
		return
	}
	if imps, ok := imports.(*ruleImports); ok && imps != nil {
		sourceRoots := slices.Clone(effective.AttrStrings("srcs"))
		deps := resolveEdges(c, ix, r, imps, from)
		retainTestRoots(c, r, from, sourceRoots)
		if !effective.ShouldKeep() && tc.programs.inputs[from.Pkg].program != nil && managedProgramRule(r, from.Pkg) {
			tc.programs.completePrograms[emissionLabel(c.RepoName, from.Pkg, ":"+from.Name)] = func(promote func(string)) {
				g := tc.programs.emission
				live := g.rules[emissionLabel(c.RepoName, from.Pkg, ":"+from.Name)]
				called := map[string]bool{}
				sourceOwner := func(dep string) emissionMode {
					key := emissionLabel(c.RepoName, from.Pkg, dep)
					called[key] = true
					if promote != nil {
						promote(key)
					}
					key, owner := tc.programs.ruleTarget(c.RepoName, key)
					if owner == nil || owner.Kind() != "ts_compile" && owner.Kind() != "ts_test" {
						return opaqueEmission
					}
					if complete := tc.programs.completePrograms[key]; complete != nil {
						delete(tc.programs.completePrograms, key)
						complete(promote)
					}
					return ruleEmission(owner)
				}
				deps := resolveEdges(c, ix, r, imps, from, sourceOwner)
				removeSuppliedProgramInputs(c, r, from, sourceRoots, sourceOwner, called, deps)
				retainSourceImporters(c, r, from, imps.program, deps)
				retainTestRoots(c, r, from, sourceRoots)
				filename := filepath.Join(c.RepoRoot, filepath.FromSlash(from.Pkg), c.DefaultBuildFileName())
				rule.MergeRules(r, live, l.Kinds()[r.Kind()].ResolveAttrs, filename)
				if promote != nil {
					for _, dep := range live.AttrStrings("deps") {
						promote(emissionLabel(c.RepoName, from.Pkg, dep))
					}
				}
				if imps.reportSrcDrops != nil {
					imps.reportSrcDrops(live)
				}
			}
		} else {
			retainSourceImporters(c, r, from, imps.program, deps)
			if imps.reportSrcDrops != nil {
				imps.reportSrcDrops(r)
			}
		}
	}
	if g := getConfig(c).programs.emission; g != nil {
		g.setResolvedRule(emissionLabel(c.RepoName, from.Pkg, ":"+from.Name), liveRule)
	}
}
