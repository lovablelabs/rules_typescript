package typescript

import (
	"errors"
	"github.com/bazelbuild/bazel-gazelle/language"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	bzl "github.com/bazelbuild/buildtools/build"

	"github.com/mikn/rules_typescript/ts/tools/jsonc"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// tsConfig is one directory's configuration, cloned down the tree; programs
// and lock are the run's, shared by every directory.
type tsConfig struct {
	protos          *protoStore
	protoIdentities map[string]*protoIdentity
	protoEnabled    []string
	// The nearest package.json above when the lockfile has no importer for it.
	foreignManifest string
	// gazelle:generation_mode update_only: a subdirectory without a BUILD file is not configured.
	updateOnly bool

	programs   *programStore
	lock       *npmLock
	lockLoaded bool
}

// getConfig retrieves the tsConfig from a config.Config. Returns a default
// tsConfig if none has been set yet (i.e. Configure was not called).
func getConfig(c *config.Config) *tsConfig {
	var tc *tsConfig
	if v, ok := c.Exts[languageName]; ok {
		tc = v.(*tsConfig)
	} else {
		tc = defaultTsConfig()
	}
	if tc.programs.repoConfig == nil {
		tc.programs.repoConfig = c.Clone()
		// Gazelle 0.51.3's Config.Clone leaves AliasMap shared.
		tc.programs.repoConfig.AliasMap = maps.Clone(c.AliasMap)
	}
	return tc
}

func defaultTsConfig() *tsConfig {
	return &tsConfig{programs: newProgramStore(), protos: newProtoStore(), protoIdentities: map[string]*protoIdentity{}}
}

func (s *programStore) recordBuild(c *config.Config, pkg string, f *rule.File) {
	if s.emission == nil {
		s.emission = &emissionGraph{roots: map[string]bool{}, outputs: map[string]bool{}, rules: map[string]*rule.Rule{}, files: map[string]*rule.File{}}
	}
	if f != nil {
		s.emission.setFile(pkg, f)
		for _, r := range f.Rules {
			s.emission.setRule(emissionLabel(c.RepoName, pkg, ":"+r.Name()), r)
		}
	}
	projected := s.buildConfig(pkg)
	if pkg != "" && c.AliasMap != nil {
		// Gazelle 0.51.3 mutates the shared parent map before this Configure boundary.
		// https://github.com/bazelbuild/bazel-gazelle/blob/v0.51.3/config/config.go
		parent := s.buildConfig(parentDir(pkg))
		clear(c.AliasMap)
		maps.Copy(c.AliasMap, parent.AliasMap)
	}
	c.AliasMap = maps.Clone(projected.AliasMap)
	c.KindMap = maps.Clone(projected.KindMap)
	ownerConfig := *c
	ownerConfig.AliasMap = projected.AliasMap
	ownerConfig.KindMap = projected.KindMap
	input := s.inputs[pkg]
	input.config = &ownerConfig
	s.inputs[pkg] = input
}

func (s *programStore) buildConfig(pkg string) *config.Config {
	var ancestors []string
	for dir := pkg; ; dir = parentDir(dir) {
		ancestors = append(ancestors, dir)
		if dir == "" {
			break
		}
	}
	c := s.repoConfig.Clone()
	c.AliasMap = maps.Clone(s.repoConfig.AliasMap)
	for _, dir := range slices.Backward(ancestors) {
		(&config.CommonConfigurer{}).Configure(c, dir, s.emission.files[dir])
	}
	for kind, mapped := range c.KindMap {
		// Gazelle 0.51.3 resolves a chain through the terminal mapping's FromKind.
		mapped.FromKind = canonicalRule(c, rule.NewRule(mapped.FromKind, "")).Kind()
		c.KindMap[kind] = mapped
	}
	return c
}

func (s *programStore) observeBuild(pkg string, dirInfo ...func(string) (walk.DirInfo, error)) {
	s.observeBuilds([]string{pkg}, dirInfo...)
}

// Loading is idempotent and fixes each rule's references, so one crawl from
// several packages loads exactly what a crawl from each would.
func (s *programStore) observeBuilds(pkgs []string, dirInfo ...func(string) (walk.DirInfo, error)) {
	if s.emission == nil {
		return
	}
	if s.index != nil {
		dirInfo = nil
	}
	if s.frozen != nil {
		s.observeFrozen(pkgs)
		return
	}
	var info func(string) (walk.DirInfo, error)
	if len(dirInfo) > 0 {
		info = dirInfo[0]
	}
	seenDirs := map[string]bool{}
	facts := s.buildFacts
	load := func(pkg string) {
		var ancestors []string
		for dir := pkg; !seenDirs[dir]; dir = parentDir(dir) {
			seenDirs[dir] = true
			ancestors = append(ancestors, dir)
			if dir == "" {
				break
			}
		}
		for _, dir := range slices.Backward(ancestors) {
			s.loadDir(dir, info, false)
		}
	}
	for _, pkg := range pkgs {
		load(pkg)
	}
	if len(dirInfo) == 0 {
		return
	}
	s.reviewCrawl(dirInfo[0])
	g := s.emission
	mapped := func(key string) string {
		if member := g.lock.memberCompilerTarget(s.repoConfig.RepoName, key); member != "" {
			return member
		}
		return key
	}
	run := &crawlRun{seen: map[string]int32{}}
	prev := g.crawl
	g.crawl = run
	defer func() { g.crawl = prev }()
	var queue []queuedLabel
	push := func(key string, origin int32) {
		key = mapped(key)
		if facts != nil {
			facts.pushed(key, origin)
		}
		if _, seen := run.seen[key]; seen {
			return
		}
		run.seen[key] = origin
		if _, hit := g.crawled[key]; !hit && !g.crawledMisses[key] {
			queue = append(queue, queuedLabel{key, origin})
		}
	}
	facts.startCrawl()
	defer facts.endCrawl()
	for dir := range seenDirs {
		if file := s.emission.files[dir]; file != nil {
			for _, stored := range file.Rules {
				key := emissionLabel(s.repoConfig.RepoName, dir, ":"+stored.Name())
				if kind := canonicalRule(s.inputs[dir].config, stored).Kind(); kind == "ts_compile" || kind == "ts_test" || kind == "ts_binary" || kind == "ts_proto_library" {
					push(key, -1)
				}
			}
		}
	}
	for len(queue) > 0 {
		key, origin := queue[0].key, queue[0].origin
		queue = queue[1:]
		if _, crawled := g.crawled[key]; crawled || g.crawledMisses[key] {
			continue
		}
		target, err := parseLabel(key)
		if err != nil || target.Repo != "" {
			g.crawledMisses[key] = true
			continue
		}
		load(target.Pkg)
		queue = append(queue, run.again...)
		run.again = run.again[:0]
		g.crawledPackage(target.Pkg)
		r := s.semanticRule(key)
		if r == nil {
			g.crawledMisses[key] = true
			continue
		}
		references, deps := crawlReferences(r)
		g.crawled[key] = crawledTarget{r.Kind(), references, deps}
		if !slices.Contains(g.crawledTargets[target.Pkg], key) {
			g.crawledTargets[target.Pkg] = append(g.crawledTargets[target.Pkg], key)
		}
		editable := facts != nil && editableAttr(r.Kind(), "deps")
		for i, text := range references {
			if text == "" {
				continue
			}
			next := emissionLabel(s.repoConfig.RepoName, target.Pkg, text)
			if read := mapped(next); !strings.HasPrefix(read, "@") && !slices.Contains(g.readers[read], key) {
				g.readers[read] = append(g.readers[read], key)
			}
			chain := origin
			if i < deps && editable {
				chain = facts.hop(key, next, origin)
			}
			push(next, chain)
		}
	}
}

func crawlReferences(r *rule.Rule) (references []string, deps int) {
	references, _ = runtimeDependencies(r)
	deps = len(references)
	references = append(references, r.AttrStrings("source_node_modules")...)
	switch r.Kind() {
	case "ts_compile", "ts_test":
		for _, attr := range inputAttrs {
			references = append(references, r.AttrStrings(attr)...)
		}
		if r.Kind() == "ts_test" {
			references = append(references, r.AttrString("runner"))
		}
	case "filegroup":
		references = append(references, r.AttrStrings("srcs")...)
	case "ts_proto_library":
		references = append(references, r.AttrString("proto"))
	}
	return references, deps
}

// reviewCrawl takes out of the memo, per directory touched since, all a crawl loaded it for if a
// load would store something there, else each crawled rule whose kind, references or deps changed.
func (s *programStore) reviewCrawl(dirInfo func(string) (walk.DirInfo, error)) {
	g := s.emission
	if g.crawledLock != g.lock {
		g.crawled = nil
	}
	g.crawlMemo()
	// No callback runs outside the root, so a disabled root is reread at every review.
	if g.pendingDisabled[""] {
		g.touchCrawled("")
	}
	// Until Resolve sets the index, every change to what a crawl reads is a callback's touch, a rule store
	// or the lockfile; a merge into a disabled directory counts as a touch at the next callback outside it.
	for dir := range g.touchedDirs {
		if s.loadDir(dir, dirInfo, true) {
			g.invalidateDir(dir)
			continue
		}
		for _, key := range g.crawledTargets[dir] {
			target, crawled := g.crawled[key]
			if !crawled {
				continue
			}
			r := s.semanticRule(key)
			if r == nil || r.Kind() != target.kind {
				g.invalidate(key)
			} else if references, deps := crawlReferences(r); deps != target.deps || !slices.Equal(references, target.references) {
				g.invalidate(key)
			}
		}
	}
	clear(g.touchedDirs)
}

// loadDir records dir's BUILD file, inputs and rules for observeBuilds, or with dryRun
// only reports whether it would record anything.
func (s *programStore) loadDir(dir string, dirInfo func(string) (walk.DirInfo, error), dryRun bool) bool {
	facts := s.buildFacts
	f := s.emission.files[dir]
	if dirInfo != nil {
		if info, err := dirInfo(dir); err == nil {
			if _, known := s.dirNames(dir); !known {
				if dryRun {
					return true
				}
				s.visit(dir, info.RegularFiles)
				facts.loadedDir(dir)
			}
			if f == nil {
				f = info.File
			}
		}
	}
	if f == nil {
		return false
	}
	if s.emission.files[dir] != f {
		if dryRun {
			return true
		}
		facts.loadedDir(dir)
		s.emission.setFile(dir, f)
	}
	if s.inputs[dir].config == nil {
		if dryRun {
			return true
		}
		s.inputs[dir] = programInput{config: s.buildConfig(dir)}
		facts.loadedDir(dir)
	}
	if s.emission.registeredRules(dir, f) {
		return false
	}
	for _, r := range f.Rules {
		key := emissionLabel(s.repoConfig.RepoName, dir, ":"+r.Name())
		if s.emission.rules[key] == nil {
			if dryRun {
				return true
			}
			s.emission.setRule(key, r)
			facts.loadedDir(dir)
		}
	}
	if !dryRun {
		s.emission.recordRegistered(dir, f)
	}
	return false
}

// clone returns a copy of the config, suitable for child directories that
// inherit from their parent.
func (tc *tsConfig) clone() *tsConfig {
	cp := *tc
	cp.protoIdentities = maps.Clone(tc.protoIdentities)
	cp.protoEnabled = slices.Clone(tc.protoEnabled)
	return &cp
}

// ---- the tsconfig.json -----------------------------------------------------

// tsConfigTargetName is the ts_config target Gazelle writes beside a package's
// own tsconfig.json, and the one a target under it names.
const tsConfigTargetName = "tsconfig"

// generatedTsConfigMarker is the _comment ts_refresh_tsconfig stamps on every
// tsconfig.json it writes (ts/private/tsconfig_aspect.bzl, _HEADER).
const generatedTsConfigMarker = "bazel run //:refresh_tsconfig"

// A tsconfig.json this ruleset writes is built out of the very targets that
// would name it, so it is never a program and never a ts_config's src.
func isGeneratedTsConfig(path string) bool {
	data, err := readInput(path)
	if err != nil {
		return false
	}
	var doc struct {
		Comment string `json:"_comment"`
	}
	if err := jsonc.Unmarshal(data, &doc); err != nil {
		return false
	}
	return strings.Contains(doc.Comment, generatedTsConfigMarker)
}

// handWrittenTsConfigIn returns the workspace-relative path of dir's own
// tsconfig.json, or "" when it has none or the one it has is generated.
func handWrittenTsConfigIn(dir, repoRoot string) string {
	candidate := filepath.Join(dir, "tsconfig.json")
	if st, err := statInput(candidate); err != nil || st.IsDir() {
		return ""
	}
	if isGeneratedTsConfig(candidate) {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, candidate)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

func isEditorProjectDir(rel string) bool {
	return strings.Contains("/"+rel+"/", "/.bazel/tsconfig/")
}

// ---- Configurer implementation ---------------------------------------------

// configureTsConfig is tsLang.Configure for one directory: the parent's config
// cloned, the codegens declared here, the program listed.
func configureTsConfig(c *config.Config, rel string, f *rule.File, dirInfo func(string) (walk.DirInfo, error)) {
	if isEditorProjectDir(rel) {
		return
	}
	tc := getConfig(c).clone()
	tc.programs.recordBuild(c, rel, f)

	if err := configureProto(c, rel, f, tc); err != nil {
		fatalf("typescript: %v", err)
	}

	// c.RepoRoot is the workspace root whichever directory Gazelle was pointed
	// at, so a run rooted below it still reads the lockfile, once.
	if !tc.lockLoaded {
		tc.lockLoaded = true
		switch l, err := loadNpmLock(c.RepoRoot, func(dir string) *manifest {
			if dirInfo != nil {
				tc.programs.observeBuild(dir, dirInfo)
			}
			if tc.programs.metadataIdentity(c, path.Join(dir, "package.json")) != authoredInput {
				return nil
			}
			return tc.programs.readManifest(c.RepoRoot, dir)
		}); {
		case err == nil:
			tc.lock = l
		case !errors.Is(err, fs.ErrNotExist):
			fatalf("typescript: %s: %v", pnpmLockfileName, err)
		}
	}

	tc.programs.recordEmissionConsumers(c, rel)
	tc.programs.emission.lock = tc.lock
	// pnpm installs nothing for a package.json the lockfile has no importer
	// for, so the project under it is foreign; an importer below ends that.
	manifest := tc.programs.metadataIdentity(c, path.Join(rel, "package.json"))
	if manifest == authoredInput {
		tc.foreignManifest = ""
		if tc.lock != nil && tc.lock.importers[rel] == nil {
			tc.foreignManifest = path.Join(rel, "package.json")
		}
	}
	if tc.foreignManifest != "" {
		tc.programs.foreign[rel] = tc.foreignManifest
	}
	if f != nil {
		for _, d := range f.Directives {
			if d.Key == "generation_mode" {
				tc.updateOnly = strings.TrimSpace(d.Value) == "update_only"
			}
		}
	}
	tc.programs.prefetchSubdirListings(c, tc, rel, dirInfo)
	if tc.programs.generatedOutput(tc.programs.index, rel, false) {
		c.Exts[languageName] = tc
		return
	}
	tc.programs.selectConfig(c, rel, f)
	switch tc.programs.metadataIdentity(c, tc.programs.configPath(rel)) {
	case generatedInput, unknownInput:
		c.Exts[languageName] = tc
		return
	}
	if tc.programs.hasAuthoredConfig(c.RepoRoot, rel) {
		if owner, ok := tc.programs.configOwner(c, tc.programs.configPath(rel)); ok && owner == rel {
			err := listTsConfigProgram(c, rel, tc, dirInfo)
			input := tc.programs.inputs[rel]
			input.discoveryErr = err
			tc.programs.inputs[rel] = input
		}
	} else if manifest == authoredInput {
		listManifestProgram(c, rel, tc)
	}

	c.Exts[languageName] = tc
}

// The files the chain's path-shaped `types` entries name, repo-relative: tsc
// resolves an inherited entry against the program's directory, not its setter's.
func typesEntryFiles(resolved *tsconfig.Resolved, rel string) []string {
	if resolved == nil || resolved.Types == nil {
		return nil
	}
	var files []string
	for _, entry := range *resolved.Types {
		entry = strings.TrimSpace(entry)
		if !strings.HasPrefix(entry, "./") && !strings.HasPrefix(entry, "../") {
			continue
		}
		if file := path.Join(rel, entry); !slices.Contains(files, file) {
			files = append(files, file)
		}
	}
	return files
}

func (s *programStore) hasAuthoredConfig(root, rel string) bool {
	file := filepath.Join(root, filepath.FromSlash(s.configPath(rel)))
	info, err := statInput(file)
	return err == nil && !info.IsDir() && !isGeneratedTsConfig(file)
}

func (s *programStore) selectConfig(c *config.Config, rel string, f *rule.File) {
	if f != nil {
		for _, r := range f.Rules {
			if canonicalRule(c, r).Kind() != "ts_config" || r.Name() != tsConfigTargetName || (!r.ShouldKeep() && !attrKept(r, "src")) {
				continue
			}
			literal, ok := r.Attr("src").(*bzl.StringExpr)
			if !ok {
				continue
			}
			src, err := label.Parse(literal.Value)
			if err != nil || literal.Value == "" {
				continue
			}
			src = sourceLabelIdentity(src.Abs(c.RepoName, rel), language.GenerateArgs{Config: c, Rel: rel})
			if src.Repo == "" {
				src.Repo = c.RepoName
			}
			if src.Canonical || src.Repo != c.RepoName || src.Pkg != rel || slices.ContainsFunc(f.Rules, func(other *rule.Rule) bool { return other.Name() == src.Name }) {
				continue
			}
			s.configs[rel] = path.Join(src.Pkg, src.Name)
		}
	}
}

func (s *programStore) readSelectedConfig(c *config.Config, rel string) bool {
	if _, known := s.configs[rel]; known {
		return true
	}
	root := c.RepoRoot
	if c.ReadBuildFilesDir != "" {
		root = c.ReadBuildFilesDir
	}
	for _, name := range c.ValidBuildFileNames {
		filename := filepath.Join(root, filepath.FromSlash(rel), name)
		if _, err := statInput(filename); os.IsNotExist(err) {
			continue
		}
		file := s.ownerBuildFiles[filename]
		if file == nil {
			var err error
			if file, err = rule.LoadFile(filename, rel); err != nil {
				fatalf("typescript: reading selected config owner %s: %v", filename, err)
			}
		}
		s.selectConfig(c, rel, file)
		if _, known := s.configs[rel]; !known {
			s.ownerBuildFiles[filename] = file
		}
		return true
	}
	return false
}
