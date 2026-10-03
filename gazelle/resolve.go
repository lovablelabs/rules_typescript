package typescript

import (
	"fmt"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
	bzl "github.com/bazelbuild/buildtools/build"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// importsForRule indexes a ts_compile or ts_test by the repository path of
// each src, what an edge target is looked up by, and a ts_codegen by its tree.
func importsForRule(c *config.Config, r *rule.Rule, f *rule.File,
	dirInfo ...func(string) (walk.DirInfo, error)) []resolve.ImportSpec {
	r = canonicalRule(c, r)
	switch r.Kind() {
	case "ts_proto_library":
		return protoImportsForRule(c, r, f)
	case "ts_codegen":
		return codegenTreeSpecs(r, f.Pkg)
	case "ts_compile", "ts_test":
	default:
		return nil
	}
	return declaredSourceImports(c, r, f, dirInfo...).imports
}

type sourceProjection struct {
	imports    []resolve.ImportSpec
	incomplete string
}

func declaredSourceImports(c *config.Config, r *rule.Rule, f *rule.File,
	dirInfo ...func(string) (walk.DirInfo, error)) sourceProjection {
	var result sourceProjection
	s := getConfig(c).programs
	active := map[label.Label]bool{}
	unknown := func(owner label.Label, attr string) {
		if result.incomplete == "" {
			result.incomplete = fmt.Sprintf("%s has unsupported %s membership", owner, attr)
		}
	}
	var visit func(string, label.Label, string)
	var list func(bzl.Expr, label.Label, string)
	list = func(expr bzl.Expr, owner label.Label, attr string) {
		if expr == nil {
			return
		}
		values, ok := expr.(*bzl.ListExpr)
		if !ok {
			unknown(owner, attr)
			return
		}
		for _, item := range values.List {
			if value, ok := item.(*bzl.StringExpr); ok {
				visit(value.Value, owner, attr)
			} else {
				unknown(owner, attr)
			}
		}
	}
	visit = func(text string, owner label.Label, attr string) {
		src, err := parseLabel(text)
		if err != nil {
			unknown(owner, attr+" label "+text)
			return
		}
		src = sourceLabelIdentity(src, language.GenerateArgs{Config: c, Rel: owner.Pkg})
		if src.Canonical || src.Repo != c.RepoName {
			unknown(owner, attr+" (external source "+src.String()+")")
			return
		}
		if active[src] {
			unknown(src, "forwarding cycle")
			return
		}
		active[src] = true
		defer delete(active, src)
		producerFile := f
		if src.Pkg != f.Pkg {
			producerFile = nil
			s.observeBuild(src.Pkg, dirInfo...)
			if s.emission != nil {
				producerFile = s.emission.files[src.Pkg]
			}
			if producerFile == nil && len(dirInfo) > 0 {
				if info, err := dirInfo[0](src.Pkg); err == nil {
					producerFile = info.File
				}
			}
		}
		var producer *rule.Rule
		if producerFile != nil {
			for _, candidate := range producerFile.Rules {
				if candidate.Name() == src.Name {
					producer = candidate
					if observed := s.semanticRule(emissionLabel(c.RepoName, src.Pkg, ":"+candidate.Name())); observed != nil {
						producer = observed
					}
					break
				}
			}
		}
		if producer == nil {
			result.imports = append(result.imports, resolve.ImportSpec{Lang: languageName, Imp: path.Join(src.Pkg, src.Name)})
			return
		}
		switch producer.Kind() {
		case "filegroup":
			if expr := producer.Attr("output_group"); expr != nil {
				if value, ok := expr.(*bzl.StringExpr); !ok || value.Value != "" {
					unknown(src, "output_group")
					return
				}
			}
			list(producer.Attr("srcs"), src, "srcs")
		case "alias":
			if actual, ok := producer.Attr("actual").(*bzl.StringExpr); ok && actual.Value != "" {
				visit(actual.Value, src, "actual")
			} else {
				unknown(src, "actual")
			}
		default:
			outputs := ruleOutputs(producer, c.RepoName, src.Pkg)
			if outputs.incomplete != "" {
				unknown(src, outputs.incomplete)
			} else if outputs.tree != "" || producer.Attr("out") == nil && producer.Attr("outs") == nil {
				unknown(src, "outputs")
			}
			for _, name := range outputs.files {
				result.imports = append(result.imports, resolve.ImportSpec{Lang: languageName, Imp: path.Join(src.Pkg, name)})
			}
		}
	}
	list(r.Attr("srcs"), label.New(c.RepoName, f.Pkg, r.Name()), "srcs")
	return result
}

func managesProgramClosure(r *rule.Rule) bool {
	return !r.ShouldKeep() && (!attrKept(r, "deps") || slices.ContainsFunc(inputAttrs, func(attr string) bool {
		return !attrKept(r, attr)
	}))
}

func automaticProgramImports(c *config.Config, consumer, sources *rule.Rule, f *rule.File,
	dirInfo ...func(string) (walk.DirInfo, error)) []resolve.ImportSpec {
	result := declaredSourceImports(c, sources, f, dirInfo...)
	ignored := slices.ContainsFunc(f.Directives, func(d rule.Directive) bool { return d.Key == "ignore" })
	closing := getConfig(c).programs.walked[f.Pkg]
	if result.incomplete != "" && closing && managesProgramClosure(consumer) && !ignored && (len(c.Langs) == 0 || slices.Contains(c.Langs, languageName)) {
		log.Fatalf("typescript: %s cannot discover its compiler closure: %s; use explicit source-file labels for automatic closure, or keep the whole rule or ignore its package and maintain sources and deps manually", label.New(c.RepoName, f.Pkg, consumer.Name()), result.incomplete)
	}
	return result.imports
}

type outputDeclarations struct {
	files      []string
	tree       string
	incomplete string
}

func ruleOutputs(r *rule.Rule, repo, pkg string) outputDeclarations {
	var outputs outputDeclarations
	for _, key := range []string{"out", "outs"} {
		var values []string
		switch expr := r.Attr(key).(type) {
		case nil:
		case *bzl.StringExpr:
			values = []string{expr.Value}
		case *bzl.ListExpr:
			for _, item := range expr.List {
				if value, ok := item.(*bzl.StringExpr); ok {
					values = append(values, value.Value)
				} else {
					outputs.incomplete = key
				}
			}
		default:
			outputs.incomplete = key
		}
		for _, value := range values {
			out, err := parseLabel(value)
			if err != nil {
				continue
			}
			out = labelIdentity(out, repo, pkg)
			if !out.Canonical && out.Repo == repo && out.Pkg == pkg {
				outputs.files = append(outputs.files, out.Name)
			}
		}
	}
	if r.Kind() == "ts_codegen" {
		switch expr := r.Attr("out_dir").(type) {
		case nil:
		case *bzl.StringExpr:
			outputs.tree = expr.Value
		default:
			outputs.incomplete = "out_dir"
		}
	}
	return outputs
}

// codegenTreeSpecs returns the ImportSpecs an out_dir ts_codegen answers to:
// the tree fills at build time, so its root is indexed for resolveCodegenTree.
func codegenTreeSpecs(r *rule.Rule, pkg string) []resolve.ImportSpec {
	outDir := ruleOutputs(r, "", pkg).tree
	if outDir == "" {
		return nil
	}
	root := path.Join(pkg, outDir)
	return []resolve.ImportSpec{
		{Lang: languageName, Imp: root},
		{Lang: languageName, Imp: codegenTreeKey(root)},
	}
}

// codegenTreeKey namespaces a codegen tree root, so the ancestor walk in
// resolveCodegenTree reaches a tree and nothing else indexed under the path.
func codegenTreeKey(root string) string {
	return "ts_codegen_tree:" + root
}

func codegenTreeOwner(s *programStore, ix *resolve.RuleIndex, imp string) (resolve.FindResult, bool) {
	if imp == "" || imp == "." || imp == ".." || !firstParty(imp) {
		return resolve.FindResult{}, false
	}
	producer, pkg, root := s.declaredOutputProducer(imp, true)
	for dir := imp; dir != "" && dir != "." && dir != "/" && dir != ".."; dir = path.Dir(dir) {
		if ix != nil {
			spec := resolve.ImportSpec{Lang: languageName, Imp: codegenTreeKey(dir)}
			for _, r := range ix.FindRulesByImport(spec, languageName) {
				return r, true
			}
		}
		if producer != nil && dir == root {
			return resolve.FindResult{Label: label.New(s.repoConfig.RepoName, pkg, producer.Name())}, true
		}
	}
	return resolve.FindResult{}, false
}

func resolveCodegenTree(s *programStore, ix *resolve.RuleIndex, imp string, from label.Label) (string, bool) {
	if owner, ok := codegenTreeOwner(s, ix, imp); ok {
		if owner.IsSelfImport(from) {
			return "", true
		}
		return relativeLabel(owner.Label, from.Repo, from.Pkg).String(), true
	}
	return "", false
}

type inputRole uint8

const (
	sourceRole inputRole = iota
	scopeRole
	compilerRole
)

var inputAttrs = []string{"srcs", "package_scopes", "type_inputs"}

type sourceInput struct {
	label string
	role  inputRole
}

func scopeInputRole(file string) inputRole {
	if isDeclarationFile(file) {
		return compilerRole
	}
	return scopeRole
}

func requiredInputScope(c *config.Config, ix *resolve.RuleIndex, file, consumer string) string {
	s := getConfig(c).programs
	if !firstParty(file) || !programCandidate(file) {
		return ""
	}
	scope := s.packageScope(c, parentDir(file))
	if scope == "" || s.generatedProgramFile(c, ix, file, true) && s.metadataIdentity(c, scope) != authoredInput {
		return ""
	}
	s.requireAuthoredScope(c, consumer, file)
	s.requireInput(c, ix, scope, file)
	return scope
}

func sourceInputLabel(c *config.Config, from label.Label, file string) (label.Label, string) {
	pkg := configSrcPackage(getConfig(c), file, "")
	name := file
	if pkg != "" {
		name = strings.TrimPrefix(name, pkg+"/")
	}
	lbl := label.New(from.Repo, pkg, name)
	return lbl, sourceLabelIdentity(lbl, language.GenerateArgs{Config: c, Rel: from.Pkg}).String()
}

func retainSourceInput(c *config.Config, from label.Label, srcs map[string]sourceInput, file string, role inputRole) {
	lbl, identity := sourceInputLabel(c, from, file)
	if previous, exists := srcs[identity]; !exists || role < previous.role {
		srcs[identity] = sourceInput{label: relativeLabel(lbl, from.Repo, from.Pkg).String(), role: role}
	}
}

type dependencyRequirement struct {
	edges         []explainfiles.Edge
	unconditional bool
}

// resolveEdges is deps from the listing: one label per edge target, by where
// it sits and who owns it. Core # gazelle:resolve names a file no rule indexes.
func resolveEdges(c *config.Config, ix *resolve.RuleIndex, r *rule.Rule,
	imps *ruleImports, from label.Label, sourceOwner ...func(string) emissionMode) map[string]dependencyRequirement {
	tc := getConfig(c)
	tc.programs.requireInstall(c.RepoRoot)
	linkedMember := false
	if tc.lock != nil && r.Kind() == "ts_compile" && r.Name() == packageName(from.Pkg) {
		for _, importer := range tc.lock.importers {
			for _, dir := range importer.links {
				if dir == from.Pkg {
					linkedMember = true
				}
			}
		}
	}
	memberEmission := opaqueEmission
	if linkedMember && len(sourceOwner) > 0 {
		memberEmission = sourceOwner[0](from.String())
	}
	sourceMember := memberEmission == sourceEmission || memberEmission == unknownEmission
	var typeEdges []explainfiles.Edge
	if p := imps.program; p != nil {
		typeEdges = p.typeEdges()
		if p.config != "" && !p.manifest {
			resolved, _ := tc.programs.resolveCompilerConfig(c, p.config, nil)
			for _, file := range typesEntryFiles(resolved, parentDir(p.config)) {
				if tc.programs.generatedProgramFile(c, ix, file, true) {
					typeEdges = append(typeEdges, explainfiles.Edge{From: p.config, To: file, Kind: explainfiles.TypeReference})
				}
			}
		}
	}
	deps := map[string]dependencyRequirement{}
	addDep := func(dep string, unconditional bool, edges ...explainfiles.Edge) {
		if dep == "" {
			return
		}
		if lbl, err := parseLabel(dep); err == nil {
			dep = relativeLabel(lbl, c.RepoName, from.Pkg).String()
		}
		required := deps[dep]
		required.edges = append(required.edges, edges...)
		required.unconditional = required.unconditional || unconditional || slices.ContainsFunc(edges, func(edge explainfiles.Edge) bool { return slices.Contains(typeEdges, edge) })
		deps[dep] = required
	}
	for _, dep := range imps.deps {
		addDep(dep, true)
	}
	reported := map[string]bool{}
	srcs := map[string]sourceInput{}
	effective := tc.programs.semanticRule(emissionLabel(c.RepoName, from.Pkg, ":"+from.Name))
	if effective == nil {
		effective = r
	}
	emit, _ := effective.Attr("emit").(*bzl.Ident)
	runtimeScopes := map[string]bool{}
	retainScope := func(scope, source string) {
		role := scopeInputRole(source)
		retainSourceInput(c, from, srcs, scope, role)
		if role == scopeRole {
			runtimeScopes[scope] = true
		}
	}
	for _, src := range effective.AttrStrings("srcs") {
		identity := src
		if lbl, err := parseLabel(src); err == nil {
			identity = sourceLabelIdentity(lbl, language.GenerateArgs{Config: c, Rel: from.Pkg}).String()
		}
		if _, exists := srcs[identity]; !exists {
			srcs[identity] = sourceInput{label: src}
		}
	}
	identityOf := func(file string) string {
		_, identity := sourceInputLabel(c, from, file)
		return identity
	}
	file := tc.programs.emission.files[from.Pkg]
	if file == nil {
		file = &rule.File{Pkg: from.Pkg}
	}
	roots := rule.NewRule("ts_compile", r.Name())
	roots.SetAttr("srcs", effective.AttrStrings("srcs"))
	for _, spec := range importsForRule(c, roots, file) {
		producer, pkg := tc.programs.outputProducer(spec.Imp)
		addDep(sourceProducerDep(producer, pkg, from), true)
		if scope := requiredInputScope(c, ix, spec.Imp, from.String()); scope != "" {
			retainScope(scope, spec.Imp)
		}
	}
	walk := func(p *program, edges []explainfiles.Edge, candidates []resolutionCandidate, configFiles map[string]bool) []explainfiles.Edge {
		byFrom := p.edgesBySource()
		candidatesByFrom := map[string][]resolutionCandidate{}
		if p != nil {
			for _, candidate := range p.candidates {
				candidatesByFrom[candidate.from] = append(candidatesByFrom[candidate.from], candidate)
			}
		}
		queue := slices.Clone(edges)
		admitCandidates := func(candidates []resolutionCandidate) {
			said := map[int]bool{}
			for _, candidate := range candidates {
				if candidate.metadata {
					switch tc.programs.metadataIdentity(c, candidate.path) {
					case generatedInput:
						log.Fatal(generatedDiscoveryConflict(candidate.path))
					case unknownInput:
						log.Fatal(tc.programs.incompleteOutput(candidate.path))
					}
					continue
				}
				if candidate.skipped == "" || said[candidate.block] {
					continue
				}
				producer, _ := tc.programs.outputProducer(candidate.path)
				if candidate.resolved == "" || producer != nil {
					said[candidate.block] = true
					log.Printf("typescript: %s imports %q: compiler skipped file lookups in absent directory %s; no scalar output selected from that directory; check the import and its compiler resolution", candidate.from, candidate.specifier, candidate.skipped)
				}
			}
		}
		admitCandidates(candidates)
		reached := map[string]bool{}
		var resolved []explainfiles.Edge
		for len(queue) > 0 {
			e := queue[0]
			queue = queue[1:]
			if linkedMember && (sourceMember || isDeclarationFile(e.From)) && configFiles == nil && strings.HasPrefix(e.From, from.Pkg+"/") &&
				strings.HasPrefix(e.Specifier, ".") && firstParty(e.To) && isDeclarationFile(e.To) &&
				!strings.HasPrefix(e.To, from.Pkg+"/") {
				log.Fatalf("typescript: workspace member %s imports foreign declaration %s from %s; npm_store_member cannot preserve this relative layout. Did you mean to move the declaration inside the member or import it through a separately published package?", from, e.To, e.From)
			}
			source := false
			if firstParty(e.From) || e.Kind == explainfiles.TypeReference && !firstParty(e.To) {
				dep, direct := edgeDep(c, ix, tc, e, from, reported, srcs, configFiles, sourceOwner...)
				source = direct
				addDep(dep, configFiles != nil, e)
				if tc.lock != nil {
					manifest := tc.programs.nearestManifest(c, parentDir(e.From))
					if name, member := tc.lock.runtimePackage(e, manifest, tc.programs.generatedProgramFile(c, ix, e.To, true)); name != "" {
						runtime := tc.lock.label(name, parentDir(e.From))
						if member {
							runtime = tc.lock.memberLabel(name, parentDir(e.From), from.Pkg)
						}
						addDep(runtime, configFiles != nil, e)
					}
				}
				resolved = append(resolved, e)
			}
			if (source || !firstParty(e.To)) && !reached[e.To] {
				reached[e.To] = true
				if source {
					if configFiles == nil {
						if scope := requiredInputScope(c, ix, e.To, e.From); scope != "" {
							retainScope(scope, e.To)
						}
						if twin := javaScriptTwin(e.To); twin != "" && srcs[identityOf(twin)].label != "" {
							if scope := requiredInputScope(c, ix, twin, e.From); scope != "" {
								retainScope(scope, twin)
							}
						}
					}
					if tc.programs.generatedProgramFile(c, ix, e.To, true) {
						continue
					}
					admitCandidates(candidatesByFrom[e.To])
				}
				queue = append(queue, byFrom[e.To]...)
			}
		}
		return resolved
	}
	walk(imps.program, imps.edges, imps.candidates, nil)
	var typeCandidates []resolutionCandidate
	if imps.program != nil {
		typeCandidates = imps.program.ownedCandidates([]string{imps.program.config})
	}
	walk(imps.program, typeEdges, typeCandidates, nil)
	if imps.config != "" {
		var configEdges []explainfiles.Edge
		files := map[string]bool{}
		if tc.programs.requireInput(c, ix, imps.config, from.String()) == generatedInput {
			if dep, _ := edgeDep(c, ix, tc, explainfiles.Edge{From: imps.config, To: imps.config}, from, reported, nil, files); dep != "" {
				addDep(dep, true)
			}
		} else {
			p := tc.programs.configProgram(c.RepoRoot, imps.config)
			var candidates []resolutionCandidate
			if p != nil {
				selected := *p
				tc.programs.selectProgram(c, &selected)
				p = &selected
				for _, candidate := range p.candidates {
					if candidate.from == imps.config {
						candidates = append(candidates, candidate)
					}
				}
			}
			configEdges = walk(p, sourceEdges(p.edgesBySource(), []string{imps.config}), candidates, files)
		}
		files[imps.config] = true
		// An emitted owner publishes a rewritten scope that staging the authored File would replace.
		emittedScopeOwner := func(scope string) string {
			if len(sourceOwner) == 0 {
				return ""
			}
			dep, held := ruleHolding(c, ix, resolve.ImportSpec{Lang: languageName, Imp: scope}, from, scopeRole)
			if !held || dep == "" || sourceOwner[0](dep) != emittedEmission {
				return ""
			}
			return dep
		}
		labels, publishers := configSrcLabels(c, ix, slices.Sorted(maps.Keys(files)), imps.config, from, emittedScopeOwner)
		for _, dep := range publishers {
			addDep(dep, true)
		}
		if len(labels) > 0 {
			r.SetAttr("config_srcs", labels)
		} else {
			r.DelAttr("config_srcs")
		}
		if dep := workersPoolAttrs(c, tc, r, imps.config, configEdges, from); dep != "" {
			addDep(dep, true)
		}
	}
	for _, scope := range slices.Sorted(maps.Keys(runtimeScopes)) {
		if len(sourceOwner) == 0 || emit == nil || emit.Name != "True" {
			continue
		}
		tc.programs.publishPackageScope(c, scope, from)
		if dep, held := ruleHolding(c, ix, resolve.ImportSpec{Lang: languageName, Imp: scope}, from, scopeRole); held && dep != "" {
			if !dependencyReaches(c, from, dep, from.String(), map[string]bool{}) {
				addDep(dep, true)
			}
		}
	}
	inputs := map[inputRole][]string{}
	for _, input := range srcs {
		inputs[input.role] = append(inputs[input.role], input.label)
	}
	for role, attr := range inputAttrs {
		values := inputs[inputRole(role)]
		if len(values) > 0 {
			slices.Sort(values)
			r.SetAttr(attr, values)
		} else if attr != "srcs" {
			r.DelAttr(attr)
		}
	}
	if len(deps) > 0 {
		r.SetAttr("deps", slices.Sorted(maps.Keys(deps)))
	}
	return deps
}

func retainSourceImporters(c *config.Config, r *rule.Rule, from label.Label, p *program, deps map[string]dependencyRequirement) {
	tc := getConfig(c)
	if tc.lock == nil {
		return
	}
	file := tc.programs.emission.files[from.Pkg]
	if file == nil {
		file = &rule.File{Pkg: from.Pkg}
	}
	effective := tc.programs.semanticRule(emissionLabel(c.RepoName, from.Pkg, ":"+from.Name))
	if effective != nil && effective.ShouldKeep() {
		return
	}
	kept := effective != nil && attrKept(effective, "source_node_modules")
	var contexts []string
	if kept {
		for _, text := range effective.AttrStrings("source_node_modules") {
			key, target := tc.programs.ruleTarget(c.RepoName, emissionLabel(from.Repo, from.Pkg, text))
			if target != nil && target.Kind() == "node_modules" {
				if owner, err := parseLabel(key); err == nil {
					contexts = append(contexts, owner.Pkg)
				}
			}
		}
	}
	inputs := rule.NewRule(r.Kind(), r.Name())
	for _, attr := range inputAttrs {
		selected := r
		if effective != nil && attrKept(effective, attr) {
			selected = effective
		}
		inputs.SetAttr(attr, selected.AttrStrings(attr))
	}
	retained := programInputFiles(c, inputs, file)
	selected := r
	if effective != nil && attrKept(effective, "deps") {
		selected = effective
	}
	finalDeps := map[string]bool{}
	for _, dep := range selected.AttrStrings("deps") {
		key, _ := tc.programs.ruleTarget(c.RepoName, emissionLabel(from.Repo, from.Pkg, dep))
		finalDeps[key] = true
	}
	requiredEdges := map[explainfiles.Edge]bool{}
	for dep, required := range deps {
		key, owner := tc.programs.ruleTarget(c.RepoName, emissionLabel(from.Repo, from.Pkg, dep))
		target, err := parseLabel(key)
		if !finalDeps[key] || err != nil || target.Repo != "npm" && (owner == nil || owner.Kind() != "node_modules_member") {
			continue
		}
		for _, edge := range required.edges {
			requiredEdges[edge] = true
		}
	}
	if p != nil {
		for _, edge := range p.Edges {
			if _, exists := retained[resolve.ImportSpec{Lang: languageName, Imp: edge.From}]; exists {
				requiredEdges[edge] = true
			}
		}
	}
	importers := map[string]bool{}
	missing := map[string]bool{}
	for edge := range requiredEdges {
		manifest := tc.programs.nearestManifest(c, parentDir(edge.From))
		generated := tc.programs.generatedProgramFile(c, tc.programs.index, edge.To, true)
		if dir, needed := tc.lock.edgeImporter(edge, manifest, from.Pkg, generated); needed {
			text := label.New(from.Repo, dir, nodeModulesTargetName).Rel(from.Repo, from.Pkg).String()
			importers[text] = true
			if kept && !slices.ContainsFunc(contexts, func(pkg string) bool {
				_, differs := tc.lock.edgeImporter(edge, manifest, pkg, generated)
				return !differs
			}) {
				missing[text] = true
			}
		}
	}
	if kept {
		if len(missing) > 0 {
			log.Fatalf("typescript: %s retains compiler inputs requiring %s, but kept source_node_modules does not supply those importer scopes; retain their node_modules targets or keep the whole rule and maintain its compiler inputs manually", from, strings.Join(slices.Sorted(maps.Keys(missing)), ", "))
		}
	} else {
		for _, text := range slices.Sorted(maps.Keys(importers)) {
			_, target := tc.programs.ruleTarget(c.RepoName, emissionLabel(from.Repo, from.Pkg, text))
			if target == nil || target.Kind() != "node_modules" {
				log.Fatalf("typescript: %s retains compiler inputs requiring importer %s, but that node_modules target is not available; include its package in this Gazelle update or declare its importer target", from, text)
			}
		}
	}
	if len(importers) > 0 {
		r.SetAttr("source_node_modules", slices.Sorted(maps.Keys(importers)))
	} else {
		r.DelAttr("source_node_modules")
	}
}

func dependencyReaches(c *config.Config, from label.Label, dep, target string, seen map[string]bool) bool {
	key, owner := programDependencyTarget(c, from, dep)
	if key == emissionLabel(c.RepoName, from.Pkg, target) {
		return true
	}
	if owner == nil || seen[key] {
		return false
	}
	seen[key] = true
	own, err := parseLabel(key)
	if err != nil {
		return false
	}
	for _, next := range owner.AttrStrings("deps") {
		if dependencyReaches(c, own, next, target, seen) {
			return true
		}
	}
	return false
}

// publisher names a scope's emitted owner, which supplies it as a dep instead of a config_srcs entry.
func configSrcLabels(c *config.Config, ix *resolve.RuleIndex, files []string, cfg string, from label.Label, publisher func(scope string) string) ([]string, []string) {
	tc := getConfig(c)
	dir := parentDir(cfg)
	retained := map[string]bool{}
	var publishers []string
	for _, f := range files {
		_, under := strings.CutPrefix(f, dir+"/")
		if dir == "" {
			under = true
		}
		if !under {
			log.Printf("typescript: %s: %s imports %s, outside the config's package "+
				"%s; a config's modules are its package's files, so no "+
				"config_srcs entry", from, cfg, f, orRepoRoot(dir))
			continue
		}
		retained[f] = true
		scope := requiredInputScope(c, ix, f, cfg)
		if scope == "" {
			continue
		}
		if publisher != nil {
			if dep := publisher(scope); dep != "" {
				publishers = append(publishers, dep)
				continue
			}
		}
		retained[scope] = true
	}
	delete(retained, cfg)
	var out []string
	for _, f := range slices.Sorted(maps.Keys(retained)) {
		pkg := configSrcPackage(tc, f, "")
		rel := strings.TrimPrefix(f, pkg+"/")
		if pkg == from.Pkg {
			if lbl, ok := srcLabel(rel); ok {
				out = append(out, lbl)
			}
			continue
		}
		out = append(out, "//"+pkg+":"+rel)
	}
	return out, publishers
}

func retainTestRoots(c *config.Config, r *rule.Rule, from label.Label, roots []string) {
	if r.Kind() != "ts_test" || r.ShouldKeep() || attrKept(r, "test_srcs") {
		return
	}
	identity := func(text string) label.Label {
		parsed, _ := parseLabel(text)
		return sourceLabelIdentity(parsed, language.GenerateArgs{Config: c, Rel: from.Pkg})
	}
	selected := map[label.Label]bool{}
	for _, root := range roots {
		selected[identity(root)] = true
	}
	for _, text := range r.AttrStrings("srcs") {
		if !selected[identity(text)] {
			r.SetAttr("test_srcs", roots)
			return
		}
	}
	r.DelAttr("test_srcs")
}

func programDependencyTarget(c *config.Config, from label.Label, dep string) (string, *rule.Rule) {
	tc := getConfig(c)
	s := tc.programs
	if s.emission == nil {
		return "", nil
	}
	key, owner := s.ruleTarget(c.RepoName, emissionLabel(c.RepoName, from.Pkg, dep))
	own, err := parseLabel(key)
	if owner == nil || err != nil {
		return key, owner
	}
	if owner.Kind() != "node_modules_member" {
		return key, owner
	}
	if member := tc.lock.memberCompilerTarget(c.RepoName, emissionLabel(c.RepoName, own.Pkg, owner.AttrString("member"))); member != "" {
		return s.ruleTarget(c.RepoName, member)
	}
	return "", nil
}

func programInputFiles(c *config.Config, r *rule.Rule, file *rule.File) map[resolve.ImportSpec]inputRole {
	inputs := map[resolve.ImportSpec]inputRole{}
	probe := rule.NewRule("ts_compile", r.Name())
	for index, attr := range inputAttrs {
		probe.SetAttr("srcs", r.AttrStrings(attr))
		role := inputRole(index)
		for _, spec := range importsForRule(c, probe, file) {
			if previous, exists := inputs[spec]; !exists || role < previous {
				inputs[spec] = role
			}
		}
	}
	return inputs
}

func suppliedProgramInputs(c *config.Config, from label.Label, dependencies []string, sourceOwner func(string) emissionMode, scopeDemands map[resolve.ImportSpec]inputRole, memo *dependencyMemo) (map[resolve.ImportSpec]inputRole, map[string]bool) {
	s := getConfig(c).programs
	supplied := map[resolve.ImportSpec]inputRole{}
	suppliedDeps := map[string]bool{}
	requiredScopes := maps.Clone(scopeDemands)
	self := emissionLabel(c.RepoName, from.Pkg, ":"+from.Name)
	var visited map[string]bool
	var collect func(label.Label, string, bool)
	var scope func(string, *rule.Rule)
	collect = func(consumer label.Label, dep string, direct bool) {
		if !direct && len(requiredScopes) == 0 {
			return
		}
		key, owner := memo.programTarget(c, consumer.Pkg, dep)
		if owner == nil || owner.Kind() != "ts_compile" {
			return
		}
		if !direct {
			scope(key, owner)
			return
		}
		supply, ok := memo.directSupply(c, from, dep, sourceOwner)
		if !ok {
			return
		}
		for _, file := range supply.files {
			supplied[resolve.ImportSpec{Lang: languageName, Imp: file}] = sourceRole
		}
		for spec, role := range supply.inputs {
			if requiredRole, wanted := requiredScopes[spec]; wanted && role <= requiredRole {
				supplied[spec] = role
				delete(requiredScopes, spec)
			}
		}
		for _, target := range supply.targets {
			suppliedDeps[target] = true
		}
	}
	scope = func(key string, owner *rule.Rule) {
		// A dependency this pass already walked can no longer change requiredScopes.
		if visited[key] && key != self {
			return
		}
		node := memo.walked[key]
		if node == nil {
			own, err := parseLabel(key)
			if err != nil {
				return
			}
			sourceOwner(key)
			owner = s.semanticRule(key)
			node = &walkedDependency{inputs: memo.programInputs(key, own.Pkg, owner)}
			memo.walked[key] = node
		}
		for spec, role := range node.inputs {
			if requiredRole, wanted := requiredScopes[spec]; wanted && role <= requiredRole {
				supplied[spec] = role
				delete(requiredScopes, spec)
			}
		}
		if visited[key] {
			return
		}
		visited[key] = true
		for _, child := range memo.compilerChildren(c, key) {
			if len(requiredScopes) == 0 {
				return
			}
			scope(child, nil)
		}
	}
	for _, dep := range dependencies {
		collect(from, dep, true)
	}
	if len(requiredScopes) > 0 {
		visited = map[string]bool{self: true}
		for _, dep := range dependencies {
			collect(from, dep, false)
		}
	}
	return supplied, suppliedDeps
}

// Minimizing one consumer re-reads every dependency per candidate; none changes until the consumer is written.
type dependencyMemo struct {
	s        *programStore
	inputs   map[string]map[resolve.ImportSpec]inputRole
	targets  map[[2]string]dependencyTarget
	programs map[[2]string]dependencyTarget
	deps     map[string][]string
	walked   map[string]*walkedDependency
	children map[string][]string
	// Set once the walk from the retained dependencies reaches only walked owners.
	walkComplete bool
}

// Read once its sourceOwner settled it: its inputs and compiler deps stay fixed.
type walkedDependency struct {
	inputs map[resolve.ImportSpec]inputRole
}

// What one direct dependency supplies; the exact and the fixed minimization both read only this.
type directSupply struct {
	key     string
	inputs  map[resolve.ImportSpec]inputRole
	files   []string
	targets []string
}

// Not memoized: sourceOwner's answer can change until every owner has settled.
func (m *dependencyMemo) directSupply(c *config.Config, from label.Label, dep string, sourceOwner func(string) emissionMode) (directSupply, bool) {
	key, owner := m.programTarget(c, from.Pkg, dep)
	if owner == nil || owner.Kind() != "ts_compile" {
		return directSupply{}, false
	}
	own, err := parseLabel(key)
	if err != nil {
		return directSupply{}, false
	}
	joined := sourceOwner(key) == sourceEmission
	owner = m.s.semanticRule(key)
	// An emitted compiler in the consumer's package publishes its borrowed sources once for both.
	packageEmitter := own.Pkg == from.Pkg && sourceOwner(key) == emittedEmission
	supply := directSupply{key: key, inputs: m.programInputs(key, own.Pkg, owner)}
	provides := false
	for spec, role := range supply.inputs {
		if role != sourceRole {
			continue
		}
		if joined || isDeclarationFile(spec.Imp) {
			supply.files = append(supply.files, spec.Imp)
			provides = true
		} else if packageEmitter && sourceExportOwner(m.s, spec.Imp) != own.Pkg {
			supply.files = append(supply.files, spec.Imp)
		}
	}
	if provides {
		if linked := m.target(c, from.Pkg, dep); linked != key {
			supply.targets = append(supply.targets, key)
		}
		supply.targets = append(supply.targets, m.dependencyTargets(c, key, own.Pkg, owner)...)
	}
	return supply, true
}

func (m *dependencyMemo) programInputs(key, pkg string, owner *rule.Rule) map[resolve.ImportSpec]inputRole {
	if inputs, ok := m.inputs[key]; ok {
		return inputs
	}
	file := m.s.emission.files[pkg]
	if file == nil {
		file = &rule.File{Pkg: pkg}
	}
	inputs := programInputFiles(m.s.inputs[pkg].config, owner, file)
	m.inputs[key] = inputs
	return inputs
}

type dependencyTarget struct {
	key   string
	owner *rule.Rule
}

func newDependencyMemo(s *programStore) *dependencyMemo {
	return &dependencyMemo{s: s, inputs: map[string]map[resolve.ImportSpec]inputRole{}, targets: map[[2]string]dependencyTarget{}, programs: map[[2]string]dependencyTarget{}, deps: map[string][]string{}, walked: map[string]*walkedDependency{}, children: map[string][]string{}}
}

func (m *dependencyMemo) target(c *config.Config, pkg, dep string) string {
	key, _ := m.targetRule(c, pkg, dep)
	return key
}

func (m *dependencyMemo) targetRule(c *config.Config, pkg, dep string) (string, *rule.Rule) {
	if target, ok := m.targets[[2]string{pkg, dep}]; ok {
		return target.key, target.owner
	}
	key, owner := m.s.ruleTarget(c.RepoName, emissionLabel(c.RepoName, pkg, dep))
	m.targets[[2]string{pkg, dep}] = dependencyTarget{key, owner}
	return key, owner
}

func (m *dependencyMemo) dependencyTargets(c *config.Config, key, pkg string, owner *rule.Rule) []string {
	if targets, ok := m.deps[key]; ok {
		return targets
	}
	var targets []string
	for _, dep := range owner.AttrStrings("deps") {
		targets = append(targets, m.target(c, pkg, dep))
	}
	m.deps[key] = targets
	return targets
}

func (m *dependencyMemo) programTarget(c *config.Config, pkg, dep string) (string, *rule.Rule) {
	if target, ok := m.programs[[2]string{pkg, dep}]; ok {
		return target.key, target.owner
	}
	key, owner := programDependencyTarget(c, label.Label{Pkg: pkg}, dep)
	m.programs[[2]string{pkg, dep}] = dependencyTarget{key, owner}
	return key, owner
}

// Recomputes the supply exactly until sourceOwner can no longer promote or complete
// a reachable owner, then minimizes over the fixed supply (fixedSupply).
func minimizeDependencies(c *config.Config, from label.Label, deps map[string]dependencyRequirement, rootInputs []resolve.ImportSpec,
	sourceOwner func(string) emissionMode, called map[string]bool, scopeDemands map[resolve.ImportSpec]inputRole, memo *dependencyMemo) []string {
	s := getConfig(c).programs
	roots := map[string]bool{}
	for _, spec := range rootInputs {
		roots[spec.Imp] = true
	}
	// A scope claim depends on dependency order; only when no edge reads a claimable scope is the supply order-free.
	orderFree := true
	for _, required := range deps {
		for _, edge := range required.edges {
			for _, file := range []string{edge.From, edge.To} {
				if _, demanded := scopeDemands[resolve.ImportSpec{Lang: languageName, Imp: file}]; demanded && !roots[file] {
					orderFree = false
				}
			}
		}
	}
	retained := map[string]bool{}
	for dep := range deps {
		retained[dep] = true
	}
	var supply *fixedSupply
	for _, candidate := range slices.Sorted(maps.Keys(retained)) {
		if orderFree && supply == nil && memo.settled(c, from, retained, called) {
			supply = newFixedSupply(c, from, deps, retained, roots, sourceOwner, scopeDemands, memo)
		}
		if supply != nil && (!supply.walks(candidate) || memo.walkSettled(c, from, retained, called)) {
			if supply.drop(candidate) {
				delete(retained, candidate)
			}
			continue
		}
		supply = nil
		delete(retained, candidate)
		supplied, suppliedDeps := suppliedProgramInputs(c, from, slices.Sorted(maps.Keys(retained)), sourceOwner, scopeDemands, memo)
		for spec := range supplied {
			if roots[spec.Imp] {
				delete(supplied, spec)
			}
		}
		supplies := func(file string) bool {
			role, exists := supplied[resolve.ImportSpec{Lang: languageName, Imp: file}]
			return exists && role == sourceRole
		}
		for dep, required := range deps {
			if retained[dep] {
				continue
			}
			key, owner := memo.targetRule(c, from.Pkg, dep)
			if required.unconditional || !suppliedDeps[key] || slices.ContainsFunc(required.edges, func(edge explainfiles.Edge) bool {
				return requiredEdgeUnsupplied(c, s, key, owner, edge, supplies)
			}) {
				retained[candidate] = true
				break
			}
		}
	}
	return slices.Sorted(maps.Keys(retained))
}

func requiredEdgeUnsupplied(c *config.Config, s *programStore, key string, owner *rule.Rule, edge explainfiles.Edge, supplies func(string) bool) bool {
	if !supplies(edge.From) {
		return true
	}
	if staticEdgeSupply(owner, edge) {
		return false
	}
	if !(isDeclarationFile(edge.To) || path.Ext(edge.To) == ".json" || supplies(edge.To)) {
		return true
	}
	return codegenEdgeUnsupplied(c, s, key, edge)
}

func staticEdgeSupply(owner *rule.Rule, edge explainfiles.Edge) bool {
	compiler := owner != nil && owner.Kind() == "ts_compile"
	member := owner != nil && owner.Kind() == "node_modules_member"
	return compiler || member || npmPackageName(edge.To) != ""
}

func codegenEdgeUnsupplied(c *config.Config, s *programStore, key string, edge explainfiles.Edge) bool {
	producer, pkg := s.outputProducer(edge.To)
	return producer == nil || producer.Kind() != "ts_codegen" || emissionLabel(c.RepoName, pkg, ":"+producer.Name()) != key
}

// settled reports whether every remaining direct owner has run sourceOwner in this
// minimization; after its first call sourceOwner promotes and completes nothing.
func (m *dependencyMemo) settled(c *config.Config, from label.Label, retained map[string]bool, called map[string]bool) bool {
	for dep := range retained {
		if key, owner := m.programTarget(c, from.Pkg, dep); owner != nil && owner.Kind() == "ts_compile" && !called[key] {
			return false
		}
	}
	return true
}

// walkSettled reports whether every owner the scope walk can reach from the
// retained dependencies has already run sourceOwner, so walking calls it for no first time.
func (m *dependencyMemo) walkSettled(c *config.Config, from label.Label, retained, called map[string]bool) bool {
	if m.walkComplete {
		return true
	}
	seen := map[string]bool{}
	var queue []string
	for dep := range retained {
		if key, owner := m.programTarget(c, from.Pkg, dep); owner != nil && owner.Kind() == "ts_compile" {
			queue = append(queue, key)
		}
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		if !called[key] {
			return false
		}
		queue = append(queue, m.compilerChildren(c, key)...)
	}
	// Retained only shrinks, so the walk stays settled.
	m.walkComplete = true
	return true
}

// compilerChildren are the compiler dependencies the scope walk descends into from a settled owner.
func (m *dependencyMemo) compilerChildren(c *config.Config, key string) []string {
	if children, ok := m.children[key]; ok {
		return children
	}
	var children []string
	if own, err := parseLabel(key); err == nil {
		if owner := m.s.semanticRule(key); owner != nil {
			for _, dep := range owner.AttrStrings("deps") {
				if child, childOwner := m.programTarget(c, own.Pkg, dep); childOwner != nil && childOwner.Kind() == "ts_compile" {
					children = append(children, child)
				}
			}
		}
	}
	m.children[key] = children
	return children
}

// fixedSupply is suppliedProgramInputs' order-free result kept as counts over the retained dependencies.
type fixedSupply struct {
	c        *config.Config
	s        *programStore
	from     label.Label
	deps     map[string]dependencyRequirement
	roots    map[string]bool
	supplies map[string]dependencySupply
	files    map[string]int
	targets  map[string]int
	claims   map[resolve.ImportSpec]int
	demands  int
	readers  map[string][]string
	needs    map[string][]string
	codegen  map[[2]string]bool
	// Only files a dependency edge reads and targets a dependency resolves to can decide a drop.
	readFiles   map[string]bool
	readTargets map[string]bool
	blocked     bool
	memo        *dependencyMemo
}

type dependencySupply struct {
	files   []string
	targets []string
	claims  []resolve.ImportSpec
}

func newFixedSupply(c *config.Config, from label.Label, deps map[string]dependencyRequirement, retained, roots map[string]bool,
	sourceOwner func(string) emissionMode, scopeDemands map[resolve.ImportSpec]inputRole, memo *dependencyMemo) *fixedSupply {
	f := &fixedSupply{c: c, s: getConfig(c).programs, from: from, deps: deps, roots: roots, memo: memo,
		supplies: map[string]dependencySupply{}, files: map[string]int{}, targets: map[string]int{}, claims: map[resolve.ImportSpec]int{},
		demands: len(scopeDemands), readers: map[string][]string{}, needs: map[string][]string{}, codegen: map[[2]string]bool{}}
	f.readFiles, f.readTargets = map[string]bool{}, map[string]bool{}
	for dep, required := range deps {
		key, _ := memo.targetRule(c, from.Pkg, dep)
		f.readTargets[key] = true
		for _, edge := range required.edges {
			f.readFiles[edge.From], f.readFiles[edge.To] = true, true
		}
	}
	for _, dep := range slices.Sorted(maps.Keys(retained)) {
		f.supplies[dep] = f.dependencySupply(dep, sourceOwner, scopeDemands)
		f.add(dep, 1)
	}
	for _, dep := range slices.Sorted(maps.Keys(deps)) {
		if retained[dep] {
			continue
		}
		f.index(dep)
		if f.unsupplied(dep) {
			// The supply only shrinks from here, so this dependency keeps every later candidate.
			f.blocked = true
		}
	}
	return f
}

func (f *fixedSupply) dependencySupply(dep string, sourceOwner func(string) emissionMode, scopeDemands map[resolve.ImportSpec]inputRole) dependencySupply {
	var supply dependencySupply
	direct, ok := f.memo.directSupply(f.c, f.from, dep, sourceOwner)
	if !ok {
		return supply
	}
	for _, file := range direct.files {
		if f.readFiles[file] {
			supply.files = append(supply.files, file)
		}
	}
	for spec, role := range direct.inputs {
		if required, wanted := scopeDemands[spec]; wanted && role <= required {
			supply.claims = append(supply.claims, spec)
		}
	}
	for _, target := range direct.targets {
		if f.readTargets[target] {
			supply.targets = append(supply.targets, target)
		}
	}
	return supply
}

func (f *fixedSupply) add(dep string, delta int) (lost []string, lostTargets []string) {
	supply := f.supplies[dep]
	for _, file := range supply.files {
		f.files[file] += delta
		if f.files[file] == 0 {
			lost = append(lost, file)
		}
	}
	for _, target := range supply.targets {
		f.targets[target] += delta
		if f.targets[target] == 0 {
			lostTargets = append(lostTargets, target)
		}
	}
	for _, spec := range supply.claims {
		f.claims[spec] += delta
	}
	return lost, lostTargets
}

// walks reports whether suppliedProgramInputs without the candidate would walk
// past the direct dependencies because some demanded scope stays unclaimed.
func (f *fixedSupply) walks(candidate string) bool {
	claimed := 0
	for _, count := range f.claims {
		if count > 0 {
			claimed++
		}
	}
	if claimed < f.demands {
		return true
	}
	for _, spec := range f.supplies[candidate].claims {
		if f.claims[spec] == 1 {
			return true
		}
	}
	return false
}

func (f *fixedSupply) supplied(file string) bool {
	return !f.roots[file] && f.files[file] > 0
}

func (f *fixedSupply) index(dep string) {
	key, owner := f.memo.targetRule(f.c, f.from.Pkg, dep)
	f.needs[key] = append(f.needs[key], dep)
	for _, edge := range f.deps[dep].edges {
		f.readers[edge.From] = append(f.readers[edge.From], dep)
		if !staticEdgeSupply(owner, edge) && !isDeclarationFile(edge.To) && path.Ext(edge.To) != ".json" {
			f.readers[edge.To] = append(f.readers[edge.To], dep)
		}
	}
}

func (f *fixedSupply) unsupplied(dep string) bool {
	required := f.deps[dep]
	key, owner := f.memo.targetRule(f.c, f.from.Pkg, dep)
	if required.unconditional || f.targets[key] == 0 {
		return true
	}
	return slices.ContainsFunc(required.edges, func(edge explainfiles.Edge) bool {
		if !f.supplied(edge.From) {
			return true
		}
		if staticEdgeSupply(owner, edge) {
			return false
		}
		if !(isDeclarationFile(edge.To) || path.Ext(edge.To) == ".json" || f.supplied(edge.To)) {
			return true
		}
		cached, known := f.codegen[[2]string{key, edge.To}]
		if !known {
			cached = codegenEdgeUnsupplied(f.c, f.s, key, edge)
			f.codegen[[2]string{key, edge.To}] = cached
		}
		return cached
	})
}

// drop removes the candidate when every dropped dependency stays supplied without
// it. Only dependencies reading what the candidate alone supplied can change.
func (f *fixedSupply) drop(candidate string) bool {
	if f.blocked {
		return false
	}
	lost, lostTargets := f.add(candidate, -1)
	affected := map[string]bool{candidate: true}
	for _, file := range lost {
		for _, dep := range f.readers[file] {
			affected[dep] = true
		}
	}
	for _, target := range lostTargets {
		for _, dep := range f.needs[target] {
			affected[dep] = true
		}
	}
	for dep := range affected {
		if f.unsupplied(dep) {
			f.add(candidate, 1)
			return false
		}
	}
	f.index(candidate)
	return true
}

// called holds every key this sourceOwner has seen; a repeated call promotes and completes nothing.
func removeSuppliedProgramInputs(c *config.Config, r *rule.Rule, from label.Label, roots []string, sourceOwner func(string) emissionMode, called map[string]bool, deps map[string]dependencyRequirement) {
	s := getConfig(c).programs
	g := s.emission
	file := g.files[from.Pkg]
	if file == nil {
		file = &rule.File{Pkg: from.Pkg}
	}
	effective := s.semanticRule(emissionLabel(c.RepoName, from.Pkg, ":"+from.Name))
	retainedRule := rule.NewRule(r.Kind(), r.Name())
	for _, attr := range inputAttrs {
		owner := r
		if effective != nil && (effective.ShouldKeep() || attrKept(effective, attr)) {
			owner = effective
		}
		retainedRule.SetAttr(attr, owner.AttrStrings(attr))
	}
	retainedInputs := programInputFiles(c, retainedRule, file)
	scopeDemands := map[resolve.ImportSpec]inputRole{}
	for spec, role := range programInputFiles(c, r, file) {
		if path.Base(spec.Imp) == "package.json" {
			scopeDemands[spec] = role
		}
	}
	dependencyLabels := r.AttrStrings("deps")
	manualDependencies := effective != nil && (effective.ShouldKeep() || attrKept(effective, "deps"))
	if manualDependencies {
		dependencyLabels = effective.AttrStrings("deps")
	}
	rootLabels := map[label.Label]bool{}
	for _, text := range roots {
		if src, err := parseLabel(text); err == nil {
			rootLabels[sourceLabelIdentity(src, language.GenerateArgs{Config: c, Rel: from.Pkg})] = true
		}
	}
	probe := rule.NewRule("ts_compile", r.Name())
	probe.SetAttr("srcs", roots)
	rootInputs := importsForRule(c, probe, file)
	memo := newDependencyMemo(s)
	if !manualDependencies {
		dependencyLabels = minimizeDependencies(c, from, deps, rootInputs, sourceOwner, called, scopeDemands, memo)
	}
	supplied, _ := suppliedProgramInputs(c, from, dependencyLabels, sourceOwner, scopeDemands, memo)
	supplies := func(spec resolve.ImportSpec, required inputRole) bool {
		role, exists := supplied[spec]
		return exists && role <= required
	}
	for _, spec := range slices.SortedFunc(maps.Keys(scopeDemands), func(a, b resolve.ImportSpec) int { return strings.Compare(a.Imp, b.Imp) }) {
		if supplies(spec, scopeDemands[spec]) {
			continue
		}
		if role, retained := retainedInputs[spec]; retained && role <= scopeDemands[spec] {
			continue
		}
		attr := inputAttrs[scopeDemands[spec]]
		role := []string{"module", "runtime scope", "compiler"}[scopeDemands[spec]]
		log.Fatalf("typescript: %s requires package scope %s, but kept %s omit it and deps do not supply its %s input; retain the scope or supply it through a compiler dependency", from, spec.Imp, attr, role)
	}
	for index, attr := range inputAttrs {
		var retained []string
		for _, text := range r.AttrStrings(attr) {
			src, err := parseLabel(text)
			if err == nil && (attr != "srcs" || !rootLabels[sourceLabelIdentity(src, language.GenerateArgs{Config: c, Rel: from.Pkg})]) {
				probe.SetAttr("srcs", []string{text})
				specs := importsForRule(c, probe, file)
				if len(specs) > 0 && !slices.ContainsFunc(specs, func(spec resolve.ImportSpec) bool { return !supplies(spec, inputRole(index)) }) {
					continue
				}
			}
			retained = append(retained, text)
		}
		if len(retained) > 0 || attr == "srcs" {
			r.SetAttr(attr, retained)
		} else {
			r.DelAttr(attr)
		}
	}
	if len(dependencyLabels) > 0 {
		r.SetAttr("deps", dependencyLabels)
	} else {
		r.DelAttr("deps")
	}
}

func configSrcPackage(tc *tsConfig, file, configDir string) string {
	s := tc.programs
	if producer, pkg := s.outputProducer(file); producer != nil {
		return pkg
	}
	for dir := parentDir(file); dir != configDir; dir = parentDir(dir) {
		if s.bazelPackage(dir) {
			return dir
		}
	}
	return configDir
}

func edgeDep(c *config.Config, ix *resolve.RuleIndex, tc *tsConfig,
	e explainfiles.Edge, from label.Label, reported map[string]bool, srcs map[string]sourceInput, configFiles map[string]bool,
	sourceOwner ...func(string) emissionMode) (dep string, source bool) {
	s := tc.programs
	readsSources := func(owner string) bool {
		mode := sourceOwner[0](owner)
		if mode == unknownEmission && !isDeclarationFile(e.To) {
			log.Fatalf("typescript: %s imports implementation %s from %s whose emit value is an expression; automatic closure cannot choose between its sources and declarations. Did you mean to give that owner a literal emit value, or keep the whole consuming rule and maintain its complete source/dependency inputs manually?", from, e.To, owner)
		}
		return mode == sourceEmission || mode != opaqueEmission && isDeclarationFile(e.To)
	}
	if !firstParty(e.To) {
		if npmPackageName(e.To) == "" {
			return "", false
		}
		if tc.lock == nil {
			if !s.noLockSaid {
				s.noLockSaid = true
				log.Printf("typescript: no %s at the repository root, so no hub "+
					"declares an npm package; npm imports get no dep", pnpmLockfileName)
			}
			return "", false
		}
		return tc.lock.edgeLabel(e, from.Pkg, tc.programs.nearestManifest(c, parentDir(e.From))), false
	}
	spec := resolve.ImportSpec{Lang: languageName, Imp: e.To}
	// Stage the declared File; its owner supplies any declared runtime closure.
	if configFiles != nil {
		if producer, _ := s.outputProducer(e.To); producer != nil {
			configFiles[e.To] = true
			owner, _ := generatedDep(c, ix, tc, e.To, from)
			return owner, false
		}
	}
	generated, generatedOwner := generatedDep(c, ix, tc, e.To, from)
	if generatedOwner {
		return generated, false
	}
	producer, pkg := s.outputProducer(e.To)
	generatedFile := producer != nil
	if !generatedFile {
		s.requireInput(c, ix, e.To, e.From)
		if configFiles != nil {
			configFiles[e.To] = true
		}
		owner, held := generated, generated != ""
		if !held {
			if tc.lock != nil {
				if lbl, ok := tc.lock.memberView(e.Specifier, e.From, from.Pkg, tc.programs.nearestManifest(c, parentDir(e.From))); ok {
					if configFiles == nil && len(sourceOwner) > 0 {
						if key, owner := programDependencyTarget(c, from, lbl); owner != nil {
							return lbl, readsSources(key)
						}
					}
					return lbl, configFiles != nil
				}
			}
			owner, held = ruleHolding(c, ix, spec, from, sourceRole)
		}
		if held {
			if owner != "" && configFiles == nil && len(sourceOwner) > 0 {
				return owner, readsSources(owner)
			}
			return owner, owner == "" && srcs != nil || configFiles != nil
		}
		if configFiles != nil {
			return "", true
		}
	}
	if srcs != nil && (slices.Contains(tsSourceExtensions, path.Ext(e.To)) ||
		slices.Contains([]string{".js", ".jsx", ".mjs", ".cjs", ".json"}, path.Ext(e.To))) {
		file := generatedFile
		if !file {
			st, err := os.Stat(filepath.Join(c.RepoRoot, filepath.FromSlash(e.To)))
			file = err == nil && st.Mode().IsRegular()
		}
		if file {
			if !isDeclarationFile(e.To) && !slices.Contains([]string{".ts", ".tsx", ".js", ".mjs", ".cjs", ".json"}, path.Ext(e.To)) {
				input := e.To
				if generatedFile {
					input = "generated " + input + " from " + label.New(from.Repo, pkg, producer.Name()).String()
				}
				log.Fatalf("typescript: %s imports %s, which ts_compile cannot accept as a source; use .ts/.tsx, .js/.mjs/.cjs, JSON or declarations, or use # gazelle:resolve typescript %s <owner label> for an existing compatible dependency", from, input, e.To)
			}
			name := e.To
			if pkg := configSrcPackage(tc, e.To, ""); pkg != "" {
				name = strings.TrimPrefix(name, pkg+"/")
			}
			if _, ok := srcLabel(name); !ok {
				reportEdge(from, e, "file name contains ':'; no Bazel label can name it", reported)
				return "", false
			}
			retainSourceInput(c, from, srcs, e.To, sourceRole)
			if twin := javaScriptTwin(e.To); twin != "" && !generatedFile && s.exportedTo(c, twin, from) {
				retainSourceInput(c, from, srcs, twin, sourceRole)
			}
			return generated, true
		}
	}
	reportEdge(from, e, s.whyUnowned(e.To), reported)
	return "", false
}

// The listing names a declaration, never its same-stem twin; only an explicit export makes the twin a runtime input.
func (s *programStore) exportedTo(c *config.Config, file string, consumer label.Label) bool {
	if s.emission == nil || !s.sourceFile(file) {
		return false
	}
	if producer, _ := s.outputProducer(file); producer != nil {
		return false
	}
	pkg := configSrcPackage(getConfig(c), file, "")
	name := strings.TrimPrefix(file, pkg+"/")
	visibility, exported, err := s.sourceVisibility(c, s.emission.files[pkg], name)
	if err != nil {
		log.Fatalf("typescript: %s cannot retain %s beside its declaration: %v. Did you mean to use literal source exports with visibility for the consuming package?", consumer, file, err)
	}
	return exported && !scopeVisibilityDenies(visibility, label.New(consumer.Repo, pkg, name), consumer)
}

func generatedDep(c *config.Config, ix *resolve.RuleIndex, tc *tsConfig, file string, from label.Label) (string, bool) {
	spec := resolve.ImportSpec{Lang: languageName, Imp: file}
	if overridden, ok := resolve.FindRuleWithOverride(c, spec, languageName); ok {
		identity := tc.programs.inputIdentity(c, ix, file)
		if identity != unavailableInput {
			return relativeLabel(overridden, from.Repo, from.Pkg).String(), identity == generatedInput
		}
	}
	producer, pkg := tc.programs.outputProducer(file)
	if producer != nil {
		if owner, held := ruleHolding(c, ix, spec, from, sourceRole); held {
			return owner, true
		}
		producerLabel := label.New(from.Repo, pkg, producer.Name())
		if rawTypeScript(file) || slices.Contains([]string{".js", ".mjs", ".cjs"}, path.Ext(file)) || producer.Kind() != "ts_codegen" {
			return sourceProducerDep(producer, pkg, from), false
		}
		if producerLabel == from {
			return "", true
		}
		return relativeLabel(producerLabel, from.Repo, from.Pkg).String(), true
	}
	if lbl := resolveProtoOutput(c, ix, file, from); lbl != "" {
		return lbl, true
	}
	return resolveCodegenTree(tc.programs, ix, file, from)
}

func sourceProducerDep(producer *rule.Rule, pkg string, from label.Label) string {
	if producer == nil || producer.Kind() != "ts_codegen" {
		return ""
	}
	owner := label.New(from.Repo, pkg, producer.Name())
	if owner == from {
		return ""
	}
	return relativeLabel(owner, from.Repo, from.Pkg).String()
}

func keptSourceRule(r *rule.Rule, file *rule.File) *rule.Rule {
	retained := r
	if !r.ShouldKeep() && r.Attr("srcs") != nil {
		retained = rule.NewRule(r.Kind(), r.Name())
		retained.SetAttr("srcs", r.Attr("srcs"))
		*retained.AttrComments("srcs") = *r.AttrComments("srcs")
		rule.MergeRules(rule.NewRule(r.Kind(), r.Name()), retained, map[string]bool{"srcs": true}, file.Path)
	}
	return retained
}

func keptRuleImports(c *config.Config, r *rule.Rule, file *rule.File) []resolve.ImportSpec {
	return importsForRule(c, keptSourceRule(r, file), file)
}

func selectedRuleImports(c *config.Config, ix *resolve.RuleIndex, r *rule.Rule, pkg string) map[resolve.ImportSpec]bool {
	r = canonicalRule(c, r)
	s := getConfig(c).programs
	file := s.emission.files[pkg]
	if file == nil {
		file = &rule.File{Pkg: pkg}
	}
	ignored := func(file *rule.File) bool {
		return file != nil && slices.ContainsFunc(file.Directives, func(d rule.Directive) bool { return d.Key == "ignore" })
	}
	imports := map[resolve.ImportSpec]bool{}
	managed := s.walked[pkg] && managedProgramRule(r, pkg) && !ignored(file) &&
		(len(c.Langs) == 0 || slices.Contains(c.Langs, languageName))
	if managed {
		for _, spec := range keptRuleImports(c, r, file) {
			imports[spec] = true
		}
	}
	for _, spec := range importsForRule(c, r, file) {
		if !managed {
			imports[spec] = true
			continue
		}
		if s.generatedProgramFile(c, ix, spec.Imp, true) {
			continue
		}
		dir := parentDir(spec.Imp)
		nearest := s.nearestPackage(dir)
		if !within(spec.Imp, pkg) || nearest != pkg && !dirIsAncestorOf(nearest, pkg) || !s.sourceFile(spec.Imp) || s.foreign[dir] != "" {
			continue
		}
		if ignored(s.emission.files[dir]) || s.generationDisabled(dir) {
			continue
		}
		imports[spec] = true
	}
	r.SetPrivateAttr("_ts_selected_imports", imports)
	return imports
}

func ruleHolding(c *config.Config, ix *resolve.RuleIndex, spec resolve.ImportSpec,
	from label.Label, required inputRole) (lbl string, held bool) {
	tc := getConfig(c)
	s := tc.programs
	scopePackage := ""
	if required == scopeRole {
		scopePackage = configSrcPackage(tc, spec.Imp, "")
	}
	eligible := func(found resolve.FindResult) bool {
		owner := s.semanticRule(emissionLabel(c.RepoName, found.Label.Pkg, ":"+found.Label.Name))
		if (required == scopeRole || !found.IsSelfImport(from)) && (owner == nil || owner.Kind() != "ts_compile") {
			return false
		}
		if required == scopeRole && found.Label.Pkg != scopePackage {
			return false
		}
		if owner == nil {
			return true
		}
		imports, selected := owner.PrivateAttr("_ts_selected_imports").(map[resolve.ImportSpec]bool)
		if !selected {
			imports = selectedRuleImports(s.inputs[found.Label.Pkg].config, ix, owner, found.Label.Pkg)
		}
		if imports[spec] {
			return true
		}
		if required == sourceRole {
			return false
		}
		file := s.emission.files[found.Label.Pkg]
		if file == nil {
			file = &rule.File{Pkg: found.Label.Pkg}
		}
		role, present := programInputFiles(s.inputs[found.Label.Pkg].config, owner, file)[spec]
		return present && role != sourceRole && role <= required
	}
	owners := slices.DeleteFunc(ix.FindRulesByImport(spec, languageName), func(found resolve.FindResult) bool { return !eligible(found) })
	if len(owners) == 0 && s.emission != nil {
		for key := range s.emission.rules {
			lbl, err := parseLabel(key)
			if err != nil {
				continue
			}
			found := resolve.FindResult{Label: sourceLabelIdentity(lbl, language.GenerateArgs{Config: c})}
			if eligible(found) {
				owners = append(owners, found)
			}
		}
	}
	slices.SortFunc(owners, func(a, b resolve.FindResult) int { return strings.Compare(a.Label.String(), b.Label.String()) })
	owners = slices.CompactFunc(owners, func(a, b resolve.FindResult) bool { return a.Label == b.Label })
	for _, r := range owners {
		if r.IsSelfImport(from) {
			return "", true
		}
	}
	if len(owners) > 0 {
		return relativeLabel(owners[0].Label, from.Repo, from.Pkg).String(), true
	}
	return "", false
}

func reportEdge(from label.Label, e explainfiles.Edge, why string,
	said map[string]bool) {
	if said[e.To] {
		return
	}
	said[e.To] = true
	log.Printf("typescript: %s: %s imports %s: %s; no dep",
		from, e.From, e.To, why)
}

func compilerConfigSource(c *config.Config, selected, owner string,
	dirInfo ...func(string) (walk.DirInfo, error)) string {
	seen := map[label.Label]bool{}
	for selected != "" {
		configLabel, err := parseLabel(selected)
		if err != nil {
			return ""
		}
		configLabel = sourceLabelIdentity(configLabel, language.GenerateArgs{Config: c, Rel: owner})
		if configLabel.Canonical || configLabel.Repo != c.RepoName || seen[configLabel] {
			return ""
		}
		seen[configLabel] = true
		s := getConfig(c).programs
		var r *rule.Rule
		if s.emission != nil {
			key := emissionLabel(c.RepoName, configLabel.Pkg, ":"+configLabel.Name)
			r = s.semanticRule(key)
			if r == nil {
				s.observeBuild(configLabel.Pkg, dirInfo...)
				r = s.semanticRule(key)
			}
		}
		if r == nil {
			if configLabel.Name == tsConfigTargetName && s.programs[configLabel.Pkg] != nil {
				return tsconfigIn(configLabel.Pkg)
			}
			return path.Join(configLabel.Pkg, configLabel.Name)
		}
		owner = configLabel.Pkg
		switch r.Kind() {
		case "ts_config":
			selected = r.AttrString("src")
			if input := s.inputs[owner]; input.program != nil && !input.program.manifest &&
				r.Name() == tsConfigTargetName && !r.ShouldKeep() && !attrKept(r, "src") {
				selected = "tsconfig.json"
			}
		case "alias":
			selected = r.AttrString("actual")
		default:
			return ""
		}
	}
	return ""
}

func (s *programStore) outputProducer(file string) (*rule.Rule, string) {
	producer, pkg, _ := s.declaredOutputProducer(file, false)
	return producer, pkg
}

func (s *programStore) declaredOutputProducer(file string, tree bool) (*rule.Rule, string, string) {
	if s.emission == nil {
		return nil, "", ""
	}
	var producer *rule.Rule
	var pkg, root string
	for dir := parentDir(file); ; dir = parentDir(dir) {
		if f := s.emission.files[dir]; f != nil {
			if outputs := s.packageOutputs(dir, f); outputs != nil {
				if tree {
					for _, t := range outputs.trees {
						if within(file, t.root) && (producer == nil || len(t.root) > len(root)) {
							producer, pkg, root = outputs.producers[t.rule], dir, t.root
						}
					}
				} else if i, ok := outputs.files[file]; ok {
					return outputs.producers[i], dir, file
				}
			} else {
				for _, r := range f.Rules {
					key := emissionLabel(s.repoConfig.RepoName, dir, ":"+r.Name())
					if s.emission.rules[key] == nil {
						s.observeBuild(dir)
					}
					r = s.semanticRule(key)
					outputs := ruleOutputs(r, s.repoConfig.RepoName, dir)
					if tree {
						candidate := path.Join(dir, outputs.tree)
						if outputs.tree != "" && within(file, candidate) && (producer == nil || len(candidate) > len(root)) {
							producer, pkg, root = r, dir, candidate
						}
						continue
					}
					for _, output := range outputs.files {
						if path.Join(dir, output) == file {
							return r, dir, file
						}
					}
				}
			}
		}
		if dir == "" {
			return producer, pkg, root
		}
	}
}

// Valid while the package's rules, their stored owners and its configuration stay the same objects.
type packageOutputs struct {
	file      *rule.File
	rules     []*rule.Rule
	config    *config.Config
	version   [2]int
	epoch     int
	producers []*rule.Rule
	files     map[string]int
	trees     []outputTree
	// The first rule whose output declarations are not literal, or -1.
	incomplete int
}

type outputTree struct {
	root string
	rule int
}

// Ownership queries run per program file; rescanning every ancestor's rules made them cubic.
// Nil while a rule is unobserved: the caller's ordered scan owns that side effect.
func (s *programStore) packageOutputs(dir string, f *rule.File) *packageOutputs {
	g := s.emission
	cfg := s.inputs[dir].config
	if cached := g.outputIndex[dir]; cached != nil && cached.file == f && cached.version == g.version(dir) &&
		cached.config == cfg && (cached.epoch == g.listEpoch || slices.Equal(cached.rules, f.Rules)) {
		cached.epoch = g.listEpoch
		return cached
	}
	index := &packageOutputs{file: f, rules: slices.Clone(f.Rules), config: cfg, version: g.version(dir), epoch: g.listEpoch, files: map[string]int{}, incomplete: -1}
	for i, r := range f.Rules {
		key := emissionLabel(s.repoConfig.RepoName, dir, ":"+r.Name())
		if g.rules[key] == nil {
			return nil
		}
		producer := s.semanticRule(key)
		index.producers = append(index.producers, producer)
		outputs := ruleOutputs(producer, s.repoConfig.RepoName, dir)
		if outputs.incomplete != "" && index.incomplete < 0 {
			index.incomplete = i
		}
		if outputs.tree != "" {
			index.trees = append(index.trees, outputTree{root: path.Join(dir, outputs.tree), rule: i})
		}
		for _, output := range outputs.files {
			if _, declared := index.files[path.Join(dir, output)]; !declared {
				index.files[path.Join(dir, output)] = i
			}
		}
	}
	if g.outputIndex == nil {
		g.outputIndex = map[string]*packageOutputs{}
	}
	g.outputIndex[dir] = index
	return index
}
