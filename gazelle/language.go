// Package typescript is the Gazelle language for rules_typescript: a package
// per tsconfig.json program, its targets from tsgo's listing.
package typescript

import (
	"flag"
	"fmt"
	"iter"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"

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
	fs.StringVar(&listingCacheDir, "ts_listing_cache", "",
		"a directory that keeps tsgo listings across runs, each reused while everything it read is unchanged")
	fs.StringVar(&resolveRecordMode, "ts_resolve_record", "on",
		"with -ts_listing_cache on a full fix or diff run: on writes back the last run's resolved BUILD files when "+
			"Resolve would start from the same state; verify replays them, then fails, drops the record and resolves in full if a "+
			"full pass with the record off would write, diff or log otherwise; off always resolves")
	fs.StringVar(&runRecordMode, "ts_run_record", "on",
		"with -ts_listing_cache on a full fix or diff run: on exits at once when every input matches the last run that changed nothing; "+
			"verify then runs a full pass without caches in -mode=diff and fails on any difference; off always runs in full")
	fs.StringVar(&fastPathMode, "ts_fastpath", "on",
		"with -ts_listing_cache on a full fix or diff run whose run record no longer matches: on replays the record when every change "+
			"is a source edit whose new imports the rules already resolve; verify then runs the full pass and fails on any difference; off never does")
	fs.BoolVar(&listingPatchVerify, "ts_listing_patch_verify", false,
		"also list with tsgo every stale listing patched and every files list listed through its config's include, and fail on any difference")
	fs.StringVar(&listingInProcessMode, "ts_listing_inproc", "on",
		"on: list in this process through the typescript-go linked in when tsgo is a release of the same TypeScript version, "+
			"else through tsgo; verify: also list each through tsgo and fail on any difference; off: always list through tsgo")
	fs.StringVar(&parallelMode, "ts_parallel", "on",
		"with -ts_listing_cache on a full recursive run, on: decode the cached listings on a worker pool ahead of the walk; "+
			"off: decode each when the walk reaches it; verify: read each again when the walk reaches it and exit on any difference")
	l.programs = tc.programs
	c.Exts[languageName] = tc
}

func (l *tsLang) CheckFlags(fs *flag.FlagSet, c *config.Config) error {
	listingVerbose = getConfig(c).programs.verbose
	if gazelleWritesFirst() {
		getConfig(c).programs.early = &earlyFinish{}
	}
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
		// Gazelle 2's default -r=auto recurses only when no directory is named.
		mode := recursive.Value.String()
		store.recursive = mode == "true" || mode == "auto" && fs.NArg() == 0
	}
	getConfig(c).programs.fullWalk = store.recursive && slices.Equal(store.roots, []string{""})
	lazyIndex.start(c.IndexLibraries && c.IndexLazy && !getConfig(c).programs.fullWalk)
	if !getConfig(c).programs.fullWalk && listingCacheDir != "" && os.Getenv("GOGC") == "" {
		// A partial run's heap is small, and GC at the default ratio was a third of its CPU;
		// on a full run the same ratio raised peak RSS from 2.4 to 4.0 GB.
		debug.SetGCPercent(200)
	}

	if tsgo := getConfig(c).programs.tsgoFlag; tsgo != "" {
		if _, err := os.Stat(tsgo); err != nil {
			return fmt.Errorf("-ts_tsgo: %w", err)
		}
	}
	if err := checkModeFlag("ts_run_record", runRecordMode); err != nil {
		return err
	}
	if err := checkModeFlag("ts_resolve_record", resolveRecordMode); err != nil {
		return err
	}
	if err := checkModeFlag("ts_listing_inproc", listingInProcessMode); err != nil {
		return err
	}
	if err := checkModeFlag("ts_parallel", parallelMode); err != nil {
		return err
	}
	if err := checkModeFlag("ts_fastpath", fastPathMode); err != nil {
		return err
	}
	s := getConfig(c).programs
	// Only the process that does the work gets past the gate, so a replay decodes nothing.
	if err := gateOnRunRecord(fs, c, s.fullWalk); err != nil {
		return err
	}
	if err := gateOnResolveVerify(fs, c, s.fullWalk); err != nil {
		return err
	}
	traceRun("full pass")
	if listingCacheDir != "" && parallelMode != "off" && s.fullWalk {
		predecodes.start(listingCacheDir, parallelMode == "verify")
	}
	if err := startResolveRecord(fs, c, s); err != nil {
		return err
	}
	if runInputs != nil && fastPathMode != "off" {
		s.buildFacts = newBuildFactsRecorder()
		runInputs.mu.Lock()
		runInputs.builds = s.buildFacts
		runInputs.mu.Unlock()
		s.importFacts = newImportFactsRecorder(func() (outputs, generated []string) {
			if s.emission != nil {
				outputs = slices.Sorted(maps.Keys(s.emission.outputs))
			}
			for pkg := range s.generatedPackages {
				if walked, _ := s.dirWalked(pkg); walked {
					generated = append(generated, pkg)
				}
			}
			slices.Sort(generated)
			return outputs, generated
		})
	}
	return nil
}

func (l *tsLang) KnownDirectives() []string {
	return []string{"ts_proto"}
}

func (l *tsLang) Configure(c *config.Config, rel string, f *rule.File) {
	getConfig(c).programs.importFacts.configured(rel, f)
	noteDirectiveFiles(c.RepoRoot, rel, f)
	getConfig(c).programs.ruleListsMayChange()
	getConfig(c).programs.touchPackage(rel)
	configureTsConfig(c, rel, f, recordedDirInfo)
	l.programs = getConfig(c).programs
	l.programs.observeBuild(rel, recordedDirInfo)
	if len(c.Langs) > 0 && !slices.Contains(c.Langs, languageName) {
		l.programs.walked[rel] = false
	}
	info, err := recordedDirInfo(rel)
	if err != nil {
		fatalf("typescript: %s: %v", rel, err)
	}
	l.programs.visit(rel, info.RegularFiles)
	for _, base := range l.programs.bases[rel] {
		l.observeSources(slices.Values([]string{tsconfigIn(base)}))
	}
	if p := l.programs.programs[rel]; p != nil {
		kept := l.programs.keptProgramSources(c, rel, recordedDirInfo)
		l.programs.speculateSources(c, p, rel, kept)
		p = l.programs.withSources(c, p, kept, rel, false)
		l.programs.record(p)
		l.observeProgram(p)
	}
}

func (l *tsLang) observeProgram(p *program) {
	l.observeSources(func(yield func(string) bool) {
		for _, file := range p.Files {
			if !yield(file) {
				return
			}
		}
		for _, candidate := range p.candidates {
			if !yield(candidate.path) {
				return
			}
		}
	})
}

// Resolve runs after the native walk releases GetDirInfo's BUILD and exclusion facts.
func (l *tsLang) observeSources(files iter.Seq[string]) {
	observed := map[string]bool{}
	var dirs []string
	for file := range files {
		dir := parentDir(file)
		if !firstParty(file) || observed[dir] {
			continue
		}
		observed[dir] = true
		dirs = append(dirs, dir)
	}
	l.programs.observeBuilds(dirs, recordedDirInfo)
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
	getConfig(args.Config).programs.touchPackage(args.Rel)
	defer getConfig(args.Config).programs.touchPackage(args.Rel)
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
		if p := l.programs.compilerProgram(args.Config, selected, owner, recordedDirInfo); p != nil {
			l.observeProgram(p)
		}
	}
	res.RelsToIndex = append(res.RelsToIndex, lazyIndex.take()...)
	s.resolution.noteGenerated(args, res)
	s.early.noteGenerated(args, res)
	s.buildFacts.generated(args, res)
	return res
}

// lazyIndex collects, on a partial run under -index=lazy, every directory the
// extension reads, which GenerateRules hands Gazelle to index before Resolve.
var lazyIndex dirsRead

type dirsRead struct {
	mu         sync.Mutex
	dirs, seen map[string]bool
	late       map[string]string
}

func (d *dirsRead) start(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dirs, d.seen, d.late = nil, nil, nil
	if on {
		d.dirs, d.seen, d.late = map[string]bool{}, map[string]bool{}, map[string]string{}
	}
}

func (d *dirsRead) read(rel string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirs != nil && !d.seen[rel] {
		d.seen[rel] = true
		d.dirs[rel] = true
	}
}

func (d *dirsRead) unseen(rel string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen != nil && !d.seen[rel]
}

// readLate notes rel, which a generated package's Imports read first.
func (d *dirsRead) readLate(rel, pkg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.late != nil {
		d.late[rel] = pkg
	}
}

// reportLate logs, once Resolve starts, each late read no later GenerateRules handed over.
func (d *dirsRead) reportLate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, rel := range slices.Sorted(maps.Keys(d.late)) {
		log.Printf("typescript: -index=lazy: %s is first read while indexing //%s, after the last generated package, so Gazelle does not index it", rel, d.late[rel])
	}
	clear(d.late)
}

func (d *dirsRead) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.dirs) == 0 {
		return nil
	}
	rels := slices.Sorted(maps.Keys(d.dirs))
	clear(d.dirs)
	for _, rel := range rels {
		delete(d.late, rel)
	}
	return rels
}

func (l *tsLang) DoneGeneratingRules() {
	l.programs.ruleListsMayChange()
	if l.programs != nil {
		l.programs.stopPrefetch()
		dropParses()
		l.programs.reportCensus()
		l.programs.reportUnlisted()
		l.programs.reportUnowned()
		l.programs.reportStemCollisions()
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
	if l.programs == nil {
		return importsForRule(c, r, f)
	}
	dirInfo := recordedDirInfo
	if l.programs.walked[f.Pkg] {
		// A generated package's crawl already read every directory its rules name;
		// a first read here would come after its RelsToIndex.
		dirInfo = func(rel string) (walk.DirInfo, error) {
			late := lazyIndex.unseen(rel)
			info, err := recordedDirInfo(rel)
			if late && err == nil {
				lazyIndex.readLate(rel, f.Pkg)
			}
			return info, err
		}
	}
	specs := importsForRule(c, r, f, dirInfo)
	l.programs.resolution.noteImports(c, r, f, specs)
	return specs
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
	lazyIndex.reportLate()
	s := getConfig(c).programs
	if s.early != nil {
		if !s.early.done {
			l.finishBeforeWriting(ix)
		}
		return
	}
	if s.resolution.begin(s) {
		return
	}
	l.resolveRule(c, ix, r, imports, from)
}

func (l *tsLang) resolveRule(c *config.Config, ix *resolve.RuleIndex, r *rule.Rule, imports any, from label.Label) {
	s := getConfig(c).programs
	s.ruleListsMayChange()
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
	if info := tsKinds()[r.Kind()]; len(info.NonEmptyAttrs) > 0 && effective.IsEmpty(info) {
		return
	}
	if effective.PrivateAttr("_ts_scope_candidate") == true {
		return
	}
	check := func(file string) {
		if tc.programs.requireInput(c, ix, file, from.String()) == generatedInput {
			if producer, _ := tc.programs.outputProducer(file); producer == nil {
				fatalf("typescript: %s requires generated file %s, but only a generated tree or compiler owner declares it. Did you mean to declare the file as a scalar output for this attribute?", from, file)
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
				rule.MergeRules(r, live, tsKinds()[r.Kind()].ResolveAttrs, filename)
				if promote != nil {
					for _, dep := range live.AttrStrings("deps") {
						promote(emissionLabel(c.RepoName, from.Pkg, dep))
					}
				}
				if imps.srcDrops != nil {
					imps.srcDrops.report(live)
				}
			}
		} else {
			retainSourceImporters(c, r, from, imps.program, deps)
			if imps.srcDrops != nil {
				imps.srcDrops.report(r)
			}
		}
	}
	if g := getConfig(c).programs.emission; g != nil {
		g.setResolvedRule(emissionLabel(c.RepoName, from.Pkg, ":"+from.Name), liveRule)
	}
}
