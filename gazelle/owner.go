package typescript

import (
	"log"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/resolve"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// tsgo's scheme for the libs the source build embeds (internal/bundled).
const embeddedLibs = "bundled:///"

// A listed path is first-party when it lies inside the repository: not above
// it, not absolute, not a lib the compiler embeds, not under node_modules.
func firstParty(f string) bool {
	if strings.HasPrefix(f, "../") || strings.HasPrefix(f, "/") ||
		strings.HasPrefix(f, embeddedLibs) {
		return false
	}
	return f != "node_modules" && !strings.HasPrefix(f, "node_modules/") && !strings.HasSuffix(f, "/node_modules") &&
		!strings.Contains(f, "/node_modules/")
}

// record keeps a listing; a program naming a first-party file is a package.
func (s *programStore) record(p *program) {
	s.programs[p.dir] = p
	delete(s.packages, p.dir)
	files := map[string]bool{}
	for _, f := range p.Files {
		if firstParty(f) && !s.generatedFile(f, true) {
			files[f] = true
		}
	}
	if len(files) > 0 {
		s.packages[p.dir] = files
	}
}

func (s *programStore) selectProgram(c *config.Config, p *program, retained ...string) {
	generated := func(file string, scalar bool) bool {
		return s.generatedProgramFile(c, s.index, file, scalar)
	}
	type resolutionKey struct {
		from, specifier string
		block           int
	}
	key := func(candidate resolutionCandidate) resolutionKey {
		return resolutionKey{candidate.from, candidate.specifier, candidate.block}
	}
	type selection struct {
		edge  explainfiles.Edge
		probe string
	}
	selected := map[resolutionKey]selection{}
	selectFile := func(candidate resolutionCandidate, file, probe string) {
		edge := p.candidateEdge(candidate)
		edge.To = file
		selected[key(candidate)] = selection{edge: edge, probe: probe}
	}
	candidatesByFrom := map[string][]resolutionCandidate{}
	treeBoundaries := map[resolutionKey]string{}
	for _, candidate := range p.candidates {
		candidatesByFrom[candidate.from] = append(candidatesByFrom[candidate.from], candidate)
		if treeBoundaries[key(candidate)] == "" {
			if _, tree := codegenTreeOwner(s, s.index, candidate.path); tree {
				treeBoundaries[key(candidate)] = candidate.path
			}
		}
	}
	for _, candidate := range p.candidates {
		if !firstParty(candidate.path) || selected[key(candidate)].edge.To != "" {
			continue
		}
		if candidate.metadata && treeBoundaries[key(candidate)] != candidate.path {
			continue
		}
		for _, file := range []string{candidate.resolved, candidate.path} {
			if file == "" {
				continue
			}
			spec := resolve.ImportSpec{Lang: languageName, Imp: file}
			if _, overridden := resolve.FindRuleWithOverride(c, spec, languageName); overridden &&
				s.inputIdentity(c, s.index, file) != unavailableInput {
				probe := file
				if boundary := treeBoundaries[key(candidate)]; boundary != "" && file == candidate.resolved {
					owner, _ := codegenTreeOwner(s, s.index, boundary)
					resolvedOwner, resolvedTree := codegenTreeOwner(s, s.index, file)
					if !resolvedTree || resolvedOwner.Label != owner.Label {
						continue
					}
					probe = boundary
				}
				selectFile(candidate, file, probe)
				break
			}
		}
		if selected[key(candidate)].edge.To == "" && generated(candidate.path, candidate.file) {
			selectFile(candidate, candidate.path, candidate.path)
		}
	}
	replaced := func(edge explainfiles.Edge) bool {
		if edge.Kind.ModuleSpecifier() || edge.Kind == explainfiles.TypeReference {
			matched, unselected := false, false
			for _, candidate := range candidatesByFrom[edge.From] {
				if candidate.specifier != edge.Specifier || candidate.resolved != edge.To ||
					(candidate.kind == explainfiles.TypeReference) != (edge.Kind == explainfiles.TypeReference) {
					continue
				}
				matched = true
				if selected[key(candidate)].edge.To == "" {
					unselected = true
				}
			}
			// The listing omits modes: every mode must replace this target.
			return matched && !unselected
		}
		return false
	}
	filterTypes := func(entries []explainfiles.TypeEntry) []explainfiles.TypeEntry {
		return slices.DeleteFunc(slices.Clone(entries), func(entry explainfiles.TypeEntry) bool {
			return replaced(explainfiles.Edge{From: p.config, To: entry.File, Specifier: entry.Entry, Kind: explainfiles.TypeReference})
		})
	}
	p.Types, p.Implicit = filterTypes(p.Types), filterTypes(p.Implicit)
	byFrom := map[string][]explainfiles.Edge{}
	for _, edge := range p.Edges {
		if !replaced(edge) {
			byFrom[edge.From] = append(byFrom[edge.From], edge)
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(selected), func(a, b resolutionKey) int {
		if n := strings.Compare(a.from, b.from); n != 0 {
			return n
		}
		if n := strings.Compare(a.specifier, b.specifier); n != 0 {
			return n
		}
		return a.block - b.block
	}) {
		byFrom[k.from] = append(byFrom[k.from], selected[k].edge)
	}
	queue := slices.Clone(p.Roots)
	for _, edge := range byFrom[p.config] {
		if edge.Kind == explainfiles.TypeReference {
			queue = append(queue, p.config)
			break
		}
	}
	if len(retained) > 0 {
		kept := map[string]bool{}
		for _, file := range retained {
			kept[file] = true
		}
		for _, file := range p.Files {
			if kept[file] {
				queue = append(queue, file)
			}
		}
	}
	for _, edge := range p.typeEdges() {
		if !replaced(edge) {
			queue = append(queue, edge.To)
		}
	}
	reached := map[string]bool{}
	var edges []explainfiles.Edge
	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		if reached[file] || generated(file, true) {
			continue
		}
		reached[file] = true
		edges = append(edges, byFrom[file]...)
		for _, edge := range byFrom[file] {
			queue = append(queue, edge.To)
		}
	}
	p.Files = slices.DeleteFunc(slices.Clone(p.Files), func(file string) bool { return !reached[file] })
	p.Edges = edges
	var block resolutionKey
	selectedProbePassed := false
	p.candidates = slices.DeleteFunc(slices.Clone(p.candidates), func(candidate resolutionCandidate) bool {
		if key(candidate) != block {
			block, selectedProbePassed = key(candidate), false
		}
		if selectedProbePassed || !firstParty(candidate.path) {
			return true
		}
		if candidate.path == selected[block].probe {
			selectedProbePassed = true
			if candidate.metadata {
				return true
			}
		}
		if !reached[candidate.from] && !(candidate.kind == explainfiles.TypeReference && candidate.from == p.config) {
			return true
		}
		return !candidate.metadata && (candidate.skipped == "" || selected[key(candidate)].edge.To != "")
	})
}

func (s *programStore) selectInput(dir string) {
	input := s.inputs[dir]
	if input.program == nil {
		return
	}
	selected := *input.program
	s.selectProgram(input.config, &selected, s.keptProgramSources(input.config, dir)...)
	s.record(&selected)
}

// Read the completed native index before any rule resolves. Rebuild each
// projection from the compiler observation, never from an earlier selection.
func (s *programStore) selectInputs(ix *resolve.RuleIndex) {
	if s.index != nil {
		return
	}
	s.index = ix
	clear(s.compilerPrograms)
	for _, dir := range slices.Sorted(maps.Keys(s.inputs)) {
		input := s.inputs[dir]
		if input.program == nil {
			continue
		}
		p, err := s.reconcileCompilerProgram(input.config, input.program, nil)
		if err != nil {
			log.Fatal(err)
		}
		if p != input.program {
			input.program = s.withKeptSources(input.config, p, dir)
			s.inputs[dir] = input
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(s.inputs)) {
		s.selectInput(dir)
	}
	for _, dir := range slices.Sorted(maps.Keys(s.inputs)) {
		if refresh := s.inputs[dir].refresh; refresh != nil {
			refresh()
		}
	}
}

func (s *programStore) generatedProgramFile(c *config.Config, ix *resolve.RuleIndex, file string, scalar bool) bool {
	if ix == nil || ix != s.index || s.emission == nil {
		return s.generatedOutput(ix, file, scalar) || scalar && len(protoOutputProviders(c, ix, file)) > 0
	}
	key := generatedKey{c, file, scalar}
	memo := s.resolutionMemo()
	if generated, ok := memo.generated[key]; ok {
		return generated
	}
	generated := s.generatedOutput(ix, file, scalar) || scalar && len(protoOutputProviders(c, ix, file)) > 0
	memo.generated[key] = generated
	return generated
}

type generatedKey struct {
	c      *config.Config
	file   string
	scalar bool
}

type inputIdentity int

const (
	unavailableInput inputIdentity = iota
	authoredInput
	generatedInput
	unknownInput
)

func (s *programStore) inputIdentity(c *config.Config, ix *resolve.RuleIndex, file string) inputIdentity {
	if s.generatedProgramFile(c, ix, file, true) {
		return generatedInput
	}
	if s.incompleteOutput(file) != nil {
		return unknownInput
	}
	if s.sourceFile(file) {
		return authoredInput
	}
	return unavailableInput
}

func (s *programStore) generatedFile(file string, scalar bool) bool {
	return s.generatedOutput(s.index, file, scalar)
}

func (s *programStore) generatedOutput(ix *resolve.RuleIndex, file string, scalar bool) bool {
	if !firstParty(file) {
		return false
	}
	if scalar {
		if producer, _ := s.outputProducer(file); producer != nil {
			return true
		}
	}
	_, found := codegenTreeOwner(s, ix, file)
	return found
}

func (s *programStore) packageDirs() []string {
	return slices.Sorted(maps.Keys(s.packages))
}

func (s *programStore) owner(f string) (string, bool) {
	if producer, _ := s.outputProducer(f); producer != nil {
		return "", false
	}
	dir := parentDir(f)
	if !s.sourceFile(f) || s.generationDisabled(dir) || s.foreign[dir] != "" {
		return "", false
	}
	for ; ; dir = parentDir(dir) {
		if files, ok := s.packages[dir]; ok {
			if files[f] && s.walked[dir] {
				return dir, true
			}
			return "", false
		}
		if dir == "" {
			return "", false
		}
	}
}

func (s *programStore) sourceFile(f string) bool {
	return firstParty(f) && slices.Contains(s.files[parentDir(f)], path.Base(f))
}

func (s *programStore) generationDisabled(dir string) bool {
	for ; ; dir = parentDir(dir) {
		if generated, observed := s.walked[dir]; observed {
			return !generated
		}
		// update_only skips Configure for contained directories without BUILD files.
		if dir == "" {
			return false
		}
		if s.emission != nil {
			if file := s.emission.files[dir]; file != nil {
				for _, directive := range file.Directives {
					if directive.Key == "ignore" {
						return true
					}
				}
				return false
			}
		}
	}
}

func (s *programStore) requireInput(c *config.Config, ix *resolve.RuleIndex, f, from string) inputIdentity {
	identity := s.inputIdentity(c, ix, f)
	if identity == unknownInput {
		log.Fatal(s.incompleteOutput(f))
	}
	if identity == unavailableInput {
		log.Fatalf("typescript: %s imports %s: %s; remove the import or update Gazelle's exclusions", from, f, s.whyUnowned(f))
	}
	return identity
}

func parentDir(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return ""
}

type fileClass int

const (
	libraryFile fileClass = iota
	testFile
	declarationFile
)

// A declaration file, .d.mts and .d.cts included: tsc reads each as a script.
func isDeclarationFile(name string) bool {
	return strings.HasSuffix(name, ".d.ts") || strings.HasSuffix(name, ".d.mts") ||
		strings.HasSuffix(name, ".d.cts")
}

func rawTypeScript(name string) bool {
	return slices.Contains(tsSourceExtensions, path.Ext(name)) && !isDeclarationFile(name)
}

// *.test.* and *.spec.*, whatever the source extension.
func isTestFile(name string) bool {
	base := strings.TrimSuffix(name, path.Ext(name))
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".spec")
}

func classify(f string) fileClass {
	base := path.Base(f)
	switch {
	case isDeclarationFile(base):
		return declarationFile
	case isTestFile(base):
		return testFile
	}
	return libraryFile
}

// The files a package owns, by class and sorted.
type srcSet struct {
	library, test, declaration []string
}

func (a srcSet) equal(b srcSet) bool {
	return slices.Equal(a.library, b.library) && slices.Equal(a.test, b.test) &&
		slices.Equal(a.declaration, b.declaration)
}

func (s *programStore) srcs(pkg string, tc *tsConfig) srcSet {
	var set srcSet
	add := func(f string) {
		switch classify(f) {
		case declarationFile:
			set.declaration = append(set.declaration, f)
		case testFile:
			set.test = append(set.test, f)
		default:
			set.library = append(set.library, f)
		}
	}
	listed := s.packages[pkg]
	for _, f := range slices.Sorted(maps.Keys(listed)) {
		if owner, found := s.owner(f); !found || owner != pkg {
			continue
		}
		if codegenWrites(f, pkg, tc) {
			continue
		}
		add(f)
		if twin := javaScriptTwin(f); twin != "" && s.sourceFile(twin) && !listed[twin] && !codegenWrites(twin, pkg, tc) {
			add(twin)
		}
	}
	for _, list := range []*[]string{&set.library, &set.test, &set.declaration} {
		slices.Sort(*list)
	}
	return set
}

// A declaration shadows its JavaScript companion in the compiler listing.
func javaScriptTwin(f string) string {
	for _, pair := range [][2]string{
		{".d.ts", ".js"}, {".d.mts", ".mjs"}, {".d.cts", ".cjs"},
	} {
		stem, ok := strings.CutSuffix(f, pair[0])
		if !ok {
			continue
		}
		return stem + pair[1]
	}
	return ""
}

// Every first-party file some program lists and no package owns, with the
// programs that reached it, once the walk is done.
func (s *programStore) reportUnowned() {
	// A run over one subtree has not listed the packages above it.
	if s.skipped || !s.walked[""] {
		return
	}
	listedBy := map[string][]string{}
	for _, dir := range s.packageDirs() {
		for f := range s.packages[dir] {
			listedBy[f] = append(listedBy[f], tsconfigIn(dir))
		}
	}
	for _, f := range slices.Sorted(maps.Keys(listedBy)) {
		if producer, _ := s.outputProducer(f); producer != nil {
			continue
		}
		if _, found := s.owner(f); !found {
			log.Printf("typescript: %s: %s; listed by %s", f, s.whyUnowned(f),
				strings.Join(listedBy[f], ", "))
		}
	}
}

func (s *programStore) whyUnowned(f string) string {
	dir := parentDir(f)
	if _, observed := s.files[dir]; !observed {
		return "under " + dir + ", which this run did not walk (excluded or ignored)"
	}
	if !s.sourceFile(f) {
		return "excluded or ignored by Gazelle"
	}
	if m := s.foreign[dir]; m != "" {
		return "under " + parentDir(m) + ", a project pnpm installs nothing " +
			"for: " + foreignReason(m)
	}
	if !s.walked[dir] {
		return "under " + dir + ", where TypeScript rules are not being generated"
	}
	for ; ; dir = parentDir(dir) {
		if _, ok := s.packages[dir]; ok {
			return "no package owns it: " + tsconfigIn(dir) +
				", the nearest, does not list it"
		}
		if dir == "" {
			return "no package owns it: no tsconfig.json above it lists a file"
		}
	}
}

func tsconfigIn(dir string) string {
	return path.Join(dir, "tsconfig.json")
}

// Under -ts_verbose, once the walk is done: the tsconfig.json met and how
// many are packages.
func (s *programStore) reportCensus() {
	if !s.verbose || s.skipped {
		return
	}
	packages, refused := 0, 0
	for _, p := range s.programs {
		switch {
		case p.refused != "":
			refused++
		case s.packages[p.dir] != nil:
			packages++
		}
	}
	log.Printf("typescript: %d tsconfig.json: %d package%s, %d refused, "+
		"%d listing no first-party file", len(s.programs), packages,
		plural(packages, "s"), refused, len(s.programs)-packages-refused)
}
