package typescript

import (
	"context"
	"encoding/json"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/bazelbuild/rules_go/go/runfiles"
)

type emissionGraph struct {
	roots   map[string]bool
	outputs map[string]bool
	// Write through setRule and deleteRule, which version each package's rules.
	rules       map[string]*rule.Rule
	versions    map[string]int
	epoch       int
	files       map[string]*rule.File
	lock        *npmLock
	outputIndex map[string]*packageOutputs
	registered  map[string]registeredBuild
	// Bumped on entry to every callback (Gazelle merges between them) and at each rule-list edit here.
	listEpoch int
	// Bumped whenever a stored owner or BUILD file that could declare outputs changes.
	outputsGeneration int
	// Bumped at every rule store except a resolved TypeScript rule stored again unchanged.
	rulesGeneration int
	// observeBuilds' crawl memo: targets whose references were followed, and the
	// packages they read. A rule edit in one of those packages clears it.
	crawled, crawledPkgs map[string]bool
	lastTouched          string
}

func (g *emissionGraph) crawlMemo() {
	if g.crawled == nil {
		g.crawled, g.crawledPkgs = map[string]bool{}, map[string]bool{}
	}
}

// touch marks the package a callback edits. Gazelle's Fix and merge rewrite only
// the BUILD file of the directory a callback ran for, after it returns.
func (g *emissionGraph) touch(pkg string) {
	if g.crawledPkgs[pkg] || g.crawledPkgs[g.lastTouched] {
		g.crawled = nil
	}
	g.lastTouched = pkg
}

func (s *programStore) touchPackage(pkg string) {
	if s != nil && s.emission != nil {
		s.emission.touch(pkg)
	}
}

func (s *programStore) ruleListsMayChange() {
	if s != nil && s.emission != nil {
		s.emission.listEpoch++
	}
}

type registeredBuild struct {
	file    *rule.File
	rules   []*rule.Rule
	version [2]int
	epoch   int
}

func (g *emissionGraph) setRule(key string, r *rule.Rule) {
	g.rulesGeneration++
	g.storeRule(key, r)
}

// Resolve edits only resolve attributes, which no proto provider reads.
func (g *emissionGraph) setResolvedRule(key string, r *rule.Rule) {
	if g.rules[key] != r {
		g.rulesGeneration++
	}
	g.storeRule(key, r)
}

func (g *emissionGraph) storeRule(key string, r *rule.Rule) {
	if declaresOutputs(g.rules[key]) || declaresOutputs(r) {
		g.outputsGeneration++
	}
	g.rules[key] = r
	g.bump(key)
}

func (g *emissionGraph) deleteRule(key string) {
	if declaresOutputs(g.rules[key]) {
		g.outputsGeneration++
	}
	g.rulesGeneration++
	delete(g.rules, key)
	g.bump(key)
}

func (g *emissionGraph) setFile(pkg string, f *rule.File) {
	if g.files[pkg] != f {
		g.outputsGeneration++
	}
	g.files[pkg] = f
}

// The attributes ruleOutputs reads; a rule without them owns no generated file.
func declaresOutputs(r *rule.Rule) bool {
	return r != nil && (r.Attr("out") != nil || r.Attr("outs") != nil || r.Attr("out_dir") != nil)
}

func (g *emissionGraph) bump(key string) {
	own, err := parseLabel(key)
	if err != nil {
		g.epoch++
		g.crawled = nil
		return
	}
	if g.crawledPkgs[own.Pkg] {
		g.crawled = nil
	}
	if g.versions == nil {
		g.versions = map[string]int{}
	}
	g.versions[own.Pkg]++
}

func (g *emissionGraph) version(pkg string) [2]int {
	return [2]int{g.epoch, g.versions[pkg]}
}

// Registering an unchanged file's rules again would store nothing.
func (g *emissionGraph) registeredRules(pkg string, f *rule.File) bool {
	r, ok := g.registered[pkg]
	if !ok || r.file != f || r.version != g.version(pkg) || r.epoch != g.listEpoch && !slices.Equal(r.rules, f.Rules) {
		return false
	}
	r.epoch = g.listEpoch
	g.registered[pkg] = r
	return true
}

func (g *emissionGraph) recordRegistered(pkg string, f *rule.File) {
	if g.registered == nil {
		g.registered = map[string]registeredBuild{}
	}
	g.registered[pkg] = registeredBuild{file: f, rules: slices.Clone(f.Rules), version: g.version(pkg), epoch: g.listEpoch}
}

var emissionLabels = struct {
	sync.Mutex
	m map[[3]string]string
}{m: map[[3]string]string{}}

func emissionLabel(repo, pkg, name string) string {
	if name == "" {
		return ""
	}
	key := [3]string{repo, pkg, name}
	emissionLabels.Lock()
	defer emissionLabels.Unlock()
	text, ok := emissionLabels.m[key]
	if !ok {
		text = resolveEmissionLabel(repo, pkg, name)
		emissionLabels.m[key] = text
	}
	return text
}

func resolveEmissionLabel(repo, pkg, name string) string {
	l, err := parseLabel(name)
	if err != nil {
		return ""
	}
	l = labelIdentity(l, repo, pkg)
	if !l.Canonical && l.Repo == repo {
		l.Repo = ""
	}
	return l.String()
}

func (s *programStore) semanticRule(key string) *rule.Rule {
	if s.emission == nil {
		return nil
	}
	r := s.emission.rules[key]
	if r == nil {
		return nil
	}
	own, err := parseLabel(key)
	if err != nil {
		return r
	}
	return canonicalRule(s.inputs[own.Pkg].config, r)
}

func (s *programStore) ruleTarget(repo, key string) (string, *rule.Rule) {
	if key == "" {
		return "", nil
	}
	if r := s.semanticRule(key); r == nil || r.Kind() != "alias" {
		return key, r
	}
	seen := map[string]bool{}
	for key != "" && !seen[key] {
		seen[key] = true
		r := s.semanticRule(key)
		if r == nil || r.Kind() != "alias" {
			return key, r
		}
		own, err := parseLabel(key)
		if err != nil {
			break
		}
		key = emissionLabel(repo, own.Pkg, r.AttrString("actual"))
	}
	return "", nil
}

func (s *programStore) runtimeConsumerTarget(repo, key string) string {
	r := s.semanticRule(key)
	own, err := parseLabel(key)
	if r == nil || err != nil || r.ShouldKeep() {
		return ""
	}
	if r.Kind() == "ts_binary" {
		return key
	}
	if r.Kind() == "ts_test" {
		runner, _ := s.ruleTarget(repo, emissionLabel(repo, own.Pkg, r.AttrString("runner")))
		rulesRepo := "rules_typescript"
		if s.repoConfig.ModuleToApparentName != nil {
			if apparent := s.repoConfig.ModuleToApparentName("rules_typescript"); apparent != "" {
				rulesRepo = apparent
			}
		}
		canonicalRepo := runfiles.CurrentRepository()
		if runner == emissionLabel(repo, "", "@"+rulesRepo+"//ts/runners:node_test") ||
			repo == "rules_typescript" && runner == "//ts/runners:node_test" ||
			canonicalRepo != "" && canonicalRepo != "_main" && runner == "@@"+canonicalRepo+"//ts/runners:node_test" {
			return key
		}
	}
	return ""
}

type emissionMode uint8

const (
	unknownEmission emissionMode = iota
	opaqueEmission
	sourceEmission
	emittedEmission
)

func ruleEmission(r *rule.Rule) emissionMode {
	if r.Attr("emit") == nil {
		return sourceEmission
	}
	if value, literal := r.Attr("emit").(*bzl.Ident); literal {
		switch value.Name {
		case "False":
			return sourceEmission
		case "True":
			return emittedEmission
		}
	}
	return unknownEmission
}

func runtimeLabels(r *rule.Rule, attr string, repeated bool) ([]string, string) {
	expr := r.Attr(attr)
	if expr == nil {
		return nil, ""
	}
	items := []bzl.Expr{expr}
	if repeated {
		list, literal := expr.(*bzl.ListExpr)
		if !literal {
			return nil, attr
		}
		items = list.List
	}
	var labels []string
	incomplete := ""
	for _, item := range items {
		value, literal := item.(*bzl.StringExpr)
		if !literal {
			incomplete = attr
			continue
		}
		labels = append(labels, value.Value)
	}
	return labels, incomplete
}

func runtimeDependencies(r *rule.Rule) ([]string, string) {
	var attributes []string
	switch r.Kind() {
	case "ts_compile", "ts_test", "ts_proto_library", "ts_npm_package", "node_modules":
		attributes = []string{"deps"}
	case "node_modules_member":
		attributes = []string{"member"}
	case "npm_workspace_package":
		attributes = []string{"target"}
	case "alias":
		attributes = []string{"actual"}
	case "ts_binary":
		attributes = []string{"entry_point"}
	}
	var dependencies []string
	for _, attr := range attributes {
		labels, incomplete := runtimeLabels(r, attr, attr == "deps")
		dependencies = append(dependencies, labels...)
		if incomplete != "" {
			return dependencies, incomplete
		}
	}
	return dependencies, ""
}

func (l *npmLock) memberCompilerTarget(repo, key string) string {
	if l == nil || !strings.HasPrefix(key, "@npm//:") {
		return ""
	}
	if l.memberLabels == nil {
		l.memberLabels = map[string]string{}
		for _, name := range slices.Sorted(maps.Keys(l.members)) {
			label := "@npm//:" + npmPackageToLabelName(name)
			if _, taken := l.memberLabels[label]; !taken {
				l.memberLabels[label] = l.members[name]
			}
		}
	}
	if dir, ok := l.memberLabels[key]; ok {
		return emissionLabel(repo, dir, ":"+packageName(dir))
	}
	return ""
}

func (s *programStore) recordEmissionConsumers(c *config.Config, pkg string) {
	g := s.emission
	if s.metadataIdentity(c, path.Join(pkg, "package.json")) != authoredInput {
		return
	}
	raw, err := os.ReadFile(filepath.Join(c.RepoRoot, filepath.FromSlash(pkg), "package.json"))
	if err != nil {
		return
	}
	var manifest map[string]any
	if json.Unmarshal(raw, &manifest) != nil {
		return
	}
	var recordEntry func(any)
	recordEntry = func(v any) {
		switch x := v.(type) {
		case string:
			for _, suffix := range []string{".js", ".jsx", ".mjs", ".cjs", ".d.ts", ".d.mts", ".d.cts"} {
				if strings.HasSuffix(x, suffix) {
					g.outputs[path.Join(pkg, x)] = true
					break
				}
			}
		case map[string]any:
			for _, entry := range x {
				recordEntry(entry)
			}
		case []any:
			for _, entry := range x {
				recordEntry(entry)
			}
		}
	}
	for _, key := range []string{"main", "module", "types", "typings", "exports", "bin"} {
		recordEntry(manifest[key])
	}
}

func (l *tsLang) AfterResolvingDeps(_ context.Context) {
	defer logListingCache()
	if l.programs == nil || l.programs.emission == nil {
		return
	}
	l.programs.ruleListsMayChange()
	g := l.programs.emission
	repo := l.programs.repoConfig.RepoName
	for pkg, f := range g.files {
		if !l.programs.generatedPackages[pkg] {
			continue
		}
		if f.File != nil {
			f.Sync()
			l.programs.ruleListsMayChange()
		}
		for _, r := range f.Rules {
			g.setRule(emissionLabel(repo, pkg, ":"+r.Name()), r)
		}
	}
	outputOwners := map[string]string{}
	for pkg, files := range l.programs.packages {
		for source := range files {
			owner, found := l.programs.owner(source)
			if isDeclarationFile(source) || !found || owner != pkg {
				continue
			}
			ext := path.Ext(source)
			var outputs []string
			switch ext {
			case ".ts":
				outputs = []string{".js", ".d.ts"}
			case ".tsx":
				outputs = []string{".js", ".jsx", ".d.ts"}
			case ".mts":
				outputs = []string{".mjs", ".d.mts"}
			case ".cts":
				outputs = []string{".cjs", ".d.cts"}
			case ".js":
				outputs = []string{".d.ts"}
			case ".mjs":
				outputs = []string{".d.mts"}
			case ".cjs":
				outputs = []string{".d.cts"}
			}
			for _, suffix := range outputs {
				outputOwners[strings.TrimSuffix(source, ext)+suffix] = emissionLabel(repo, pkg, ":"+packageName(pkg))
			}
		}
	}
	for output := range g.outputs {
		if owner := outputOwners[output]; owner != "" {
			g.roots[owner] = true
		}
		if strings.Contains(output, "*") {
			for file, owner := range outputOwners {
				if matchesExportOutput(output, file) {
					g.roots[owner] = true
				}
			}
		}
	}
	seen := map[string]bool{}
	var visit func(string, string)
	visit = func(key, consumer string) {
		if key == "" {
			return
		}
		if required, visited := seen[key]; visited && (required || consumer == "") {
			return
		}
		seen[key] = consumer != ""
		if member := g.lock.memberCompilerTarget(repo, key); member != "" {
			visit(member, consumer)
			return
		}
		r := g.rules[key]
		if r == nil {
			return
		}
		own, err := parseLabel(key)
		if err != nil {
			return
		}
		semantic := l.programs.semanticRule(key)
		if kind := semantic.Kind(); kind == "ts_compile" || kind == "ts_test" {
			if consumer == "" && l.programs.generatedPackages[own.Pkg] && !r.ShouldKeep() {
				consumer = key
				seen[key] = true
			}
			if l.programs.generatedPackages[own.Pkg] && !r.ShouldKeep() && !attrKept(r, "emit") {
				r.SetAttr("emit", true)
				semantic = l.programs.semanticRule(key)
			}
			if consumer != "" && ruleEmission(semantic) != emittedEmission {
				declared := declaredSourceImports(l.programs.inputs[own.Pkg].config, semantic, g.files[own.Pkg])
				if declared.incomplete != "" {
					log.Fatalf("typescript: %s requires a runtime boundary from %s, but %s; source emission cannot establish whether unpublished TypeScript needs transformation. Did you mean to use explicit source-file labels or declare emit = True on the owner?", consumer, key, declared.incomplete)
				}
				for _, input := range declared.imports {
					if rawTypeScript(input.Imp) {
						log.Fatalf("typescript: %s requires emission from %s for runtime TypeScript input %s, but its effective emit value does not establish emitted JavaScript. Did you mean to include that package in this update, remove its keep marker, or declare emit = True on the owner?", consumer, key, input.Imp)
					}
				}
			}
		} else if kind == "ts_proto_library" && consumer != "" {
			log.Fatalf("typescript: %s requires emitted JavaScript from %s, but ts_proto_library publishes TypeScript with fixed emit = False. Did you mean to use a source-capable consumer or a dependency that publishes emitted JavaScript?", consumer, key)
		}
		dependencies, incomplete := runtimeDependencies(semantic)
		if consumer != "" && incomplete != "" {
			log.Fatalf("typescript: %s requires the runtime closure of %s, but its %s is an expression; automatic publication cannot establish its runtime dependencies. Did you mean to use literal dependency and forwarding labels, or keep the whole consuming rule and maintain its runtime closure manually?", consumer, key, incomplete)
		}
		for _, dep := range dependencies {
			visit(emissionLabel(repo, own.Pkg, dep), consumer)
		}
	}
	for key := range g.rules {
		if target := l.programs.runtimeConsumerTarget(repo, key); target != "" {
			consumer := ""
			own, err := parseLabel(key)
			if err == nil && l.programs.generatedPackages[own.Pkg] && !g.rules[key].ShouldKeep() {
				consumer = key
			}
			visit(target, consumer)
		}
	}
	for root := range g.roots {
		visit(root, "")
	}
	// Rooted programs can promote owners that unrooted programs would otherwise read as sources.
	for _, rooted := range []bool{true, false} {
		for {
			completed := false
			for key, complete := range l.programs.completePrograms {
				if rooted && !seen[key] {
					continue
				}
				delete(l.programs.completePrograms, key)
				var promote func(string)
				if rooted {
					promote = func(dep string) { visit(dep, key) }
				}
				complete(promote)
				completed = true
			}
			if !completed {
				break
			}
		}
	}
	l.programs.exportPackageScopes()
	for key, r := range g.rules {
		if r.PrivateAttr("_ts_scope_candidate") == true && len(r.AttrStrings("package_scopes")) == 0 {
			r.Delete()
			g.deleteRule(key)
		}
	}
	for pkg, file := range g.files {
		if l.programs.generatedPackages[pkg] && file.File != nil {
			file.Sync()
			l.programs.ruleListsMayChange()
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(g.files)) {
		l.programs.relocateSourceExports(l.programs.inputs[pkg].config, pkg)
	}
	for _, pkg := range slices.Sorted(maps.Keys(g.files)) {
		file := g.files[pkg]
		if !l.programs.generatedPackages[pkg] {
			continue
		}
		if file.File != nil {
			file.Sync()
			l.programs.ruleListsMayChange()
		}
		if file.Content == nil && l.programs.bazelPackage(pkg) {
			if err := l.programs.newPackageTargetConflict(pkg); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func (s *programStore) exportPackageScopes() {
	tc := &tsConfig{programs: s}
	pending := map[string]*rule.Rule{}
	for pkg, file := range s.emission.files {
		for _, r := range file.Rules {
			if r.PrivateAttr("_ts_source_export") != "pending" {
				continue
			}
			list, err := exportSourceMembership(r)
			if err != nil {
				log.Fatal(err)
			}
			for _, item := range list.List {
				pending[emissionLabel(s.repoConfig.RepoName, pkg, ":"+item.(*bzl.StringExpr).Value)] = r
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(s.emission.rules)) {
		r := s.emission.rules[key]
		if r.Kind() == "exports_files" {
			continue
		}
		consumer, err := parseLabel(key)
		if err != nil {
			continue
		}
		for _, attr := range []string{"srcs", "package_scopes", "type_inputs", "data", "config_srcs"} {
			for _, text := range r.AttrStrings(attr) {
				source, err := parseLabel(text)
				if err != nil {
					continue
				}
				source = labelIdentity(source, s.repoConfig.RepoName, consumer.Pkg)
				scope := path.Join(source.Pkg, source.Name)
				if source.Canonical || source.Repo != s.repoConfig.RepoName || path.Base(source.Name) != "package.json" ||
					s.inputIdentity(s.repoConfig, s.index, scope) != authoredInput {
					continue
				}
				pkg := configSrcPackage(tc, scope, "")
				if pkg == consumer.Pkg {
					continue
				}
				file := s.emission.files[pkg]
				name := strings.TrimPrefix(scope, pkg+"/")
				s.relocateSourceExports(s.repoConfig, pkg)
				visibility, exported, err := s.sourceVisibility(s.repoConfig, file, name)
				if err != nil {
					log.Fatalf("typescript: %s cannot retain package scope %s: %v. Did you mean to use literal source exports with visibility for the consuming package?", key, scope, err)
				}
				if scopeVisibilityDenies(visibility, label.New(s.repoConfig.RepoName, pkg, name), consumer) {
					log.Fatalf("typescript: %s requires package scope %s, but source visibility %v denies access. Did you mean to export that scope to the consuming package, or supply it through a compiler dependency?", key, scope, visibility)
				}
				if exported {
					continue
				}
				sourceKey := emissionLabel(s.repoConfig.RepoName, pkg, ":"+name)
				exports := pending[sourceKey]
				if !s.generatedPackages[pkg] || file == nil || exports == nil && file.File == nil {
					log.Fatalf("typescript: %s requires package scope %s without a source export in %s, which is not selected for publication. Did you mean to include %s in the Gazelle update?", key, scope, orRepoRoot(pkg), orRepoRoot(pkg))
				}
				if exports == nil {
					exports = rule.NewRule("exports_files", "")
					exports.SetAttr("srcs", []string{name})
					exports.Insert(file)
					s.ruleListsMayChange()
				}
				exports.SetAttr("visibility", visibility)
				exports.SetPrivateAttr("_ts_source_export", nil)
				delete(pending, sourceKey)
			}
		}
	}
	for _, r := range pending {
		r.Delete()
	}
}

func scopeVisibilityDenies(visibility []string, owner, consumer label.Label) bool {
	consumer = labelIdentity(consumer, owner.Repo, owner.Pkg)
	if owner.Repo == consumer.Repo && owner.Canonical == consumer.Canonical && owner.Pkg == consumer.Pkg {
		return false
	}
	for _, text := range visibility {
		grant, err := parseLabel(text)
		if err != nil {
			return false
		}
		grant = labelIdentity(grant, owner.Repo, owner.Pkg)
		switch {
		case grant.Pkg == "visibility" && grant.Name == "public":
			return false
		case grant.Pkg == "visibility" && grant.Name == "private":
		case grant.Name == "__pkg__":
			if grant.Repo == consumer.Repo && grant.Canonical == consumer.Canonical && grant.Pkg == consumer.Pkg {
				return false
			}
		case grant.Name == "__subpackages__":
			if grant.Repo == consumer.Repo && grant.Canonical == consumer.Canonical && within(consumer.Pkg, grant.Pkg) {
				return false
			}
		default:
			// Bazel evaluates package_group membership.
			return false
		}
	}
	return true
}

func (s *programStore) publishPackageScope(c *config.Config, scope string, consumer label.Label) {
	pkg := configSrcPackage(getConfig(c), scope, "")
	key := emissionLabel(c.RepoName, pkg, ":"+packageName(pkg))
	r := s.emission.rules[key]
	if !s.generatedPackages[pkg] || r == nil || r.PrivateAttr("_ts_scope_candidate") != true {
		return
	}
	scopes := r.AttrStrings("package_scopes")
	source := strings.TrimPrefix(scope, pkg+"/")
	visibility, _, err := s.sourceVisibility(c, s.emission.files[pkg], source)
	if err != nil {
		log.Fatalf("typescript: %s cannot publish package scope %s: %v. Did you mean to use literal source exports, or keep an explicit compiler owner and maintain its visibility manually?", key, scope, err)
	}
	if scopeVisibilityDenies(visibility, label.New(c.RepoName, pkg, r.Name()), consumer) {
		log.Fatalf("typescript: %s requires runtime package scope %s, but source visibility %v denies access to its automatic publisher %s. Did you mean to export that scope to the consuming package, or provide an explicit compiler owner with the required visibility?", consumer, scope, visibility, key)
	}
	for _, previous := range scopes {
		policy, _, err := s.sourceVisibility(c, s.emission.files[pkg], previous)
		if err != nil || !slices.Equal(policy, visibility) {
			log.Fatalf("typescript: %s cannot publish package scopes %s and %s with different or unknown source visibility. Did you mean to provide explicit compiler owners with the required visibility?", key, previous, source)
		}
	}
	if attrKept(r, "visibility") {
		policy, err := exportVisibility(c, r, pkg)
		if err != nil || !slices.Equal(policy, visibility) {
			log.Fatalf("typescript: %s cannot publish package scope %s through kept visibility that differs from its source export. Did you mean to remove that keep marker, or keep the whole compiler owner and maintain its complete inputs and visibility manually?", key, scope)
		}
	}
	r.SetAttr("visibility", visibility)
	if !slices.Contains(scopes, source) {
		scopes = append(scopes, source)
		slices.Sort(scopes)
		r.SetAttr("package_scopes", scopes)
	}
}

// Node replaces every RHS star with the same subpath, including slashes: https://nodejs.org/api/packages.html#subpath-patterns.
func matchesExportOutput(pattern, file string) bool {
	stars := strings.Count(pattern, "*")
	if stars == 0 {
		return pattern == file
	}
	fixed := len(pattern) - stars
	width := len(file) - fixed
	if width < 0 || width%stars != 0 {
		return false
	}
	start := strings.IndexByte(pattern, '*')
	capture := file[start : start+width/stars]
	return strings.ReplaceAll(pattern, "*", capture) == file
}
