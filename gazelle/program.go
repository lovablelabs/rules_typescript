package typescript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/walk"
	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// Linked in from gazelle/BUILD.bazel's x_defs; empty under a plain go build.
var tsgoRlocationpath string

// One tsconfig.json as tsgo lists it from the repository root, first-party
// paths relative to it and the compiler's libs under ../ or bundled:///.
type program struct {
	explainfiles.Listing
	candidates []resolutionCandidate
	config     string
	dir        string
	refused    string
	manifest   bool
	bySource   *edgeIndex
}

// One run's listings, one store every directory's config shares: the packages
// and their first-party files, each walked directory's files, the chains.
type programStore struct {
	repoConfig        *config.Config
	emission          *emissionGraph
	inputs            map[string]programInput
	completePrograms  map[string]func(promote func(string))
	index             *resolve.RuleIndex
	tsgoFlag          string
	verbose           bool
	tsgo              string
	skipped           bool
	programs          map[string]*program
	packages          map[string]map[string]bool
	generatedPackages map[string]bool
	visited           map[string][]string
	files             map[string][]string
	walked            map[string]bool
	bases             map[string][]string
	extended          map[string]bool
	// Per walked directory, the package.json that makes it a foreign project.
	foreign          map[string]string
	foreignSaid      map[string]bool
	vitestPrograms   map[string]*program
	compilerPrograms map[string]*program
	wranglerConfigs  map[string]string
	installChecked   bool
	regularFiles     map[string]bool
	memo             *resolutionMemo
	protoIndex       *protoIndex
	prefetch         *listingPrefetch
	fullWalk         bool
	manifests        map[string]*manifest
	noLockSaid       bool
}

// Every observed package retains configuration, even without a compiler program;
// compiler observations survive selection until native providers finish indexing.
type programInput struct {
	config       *config.Config
	program      *program
	discoveryErr error
	refresh      func()
}

func newProgramStore() *programStore {
	return &programStore{
		inputs:            map[string]programInput{},
		completePrograms:  map[string]func(promote func(string)){},
		programs:          map[string]*program{},
		packages:          map[string]map[string]bool{},
		generatedPackages: map[string]bool{},
		visited:           map[string][]string{},
		files:             map[string][]string{},
		walked:            map[string]bool{},
		bases:             map[string][]string{},
		extended:          map[string]bool{},
		foreign:           map[string]string{},
		foreignSaid:       map[string]bool{},
		wranglerConfigs:   map[string]string{},
	}
}

func (s *programStore) say(format string, a ...any) {
	if s.verbose {
		log.Printf("typescript: "+format, a...)
	}
}

var errNoTsgo = errors.New("no tsgo binary: run the gazelle_typescript binary, pass -ts_tsgo=<path>, or set TSGO")

func (s *programStore) binary() (string, error) {
	if s.tsgo != "" {
		return s.tsgo, nil
	}
	abs, how, err := s.locateBinary()
	if err != nil {
		return "", err
	}
	s.tsgo = abs
	s.say("listing programs with %s (%s)", abs, how)
	return abs, nil
}

// findBinary is binary without its one report.
func (s *programStore) findBinary() (string, error) {
	if s.tsgo != "" {
		return s.tsgo, nil
	}
	abs, _, err := s.locateBinary()
	return abs, err
}

func (s *programStore) locateBinary() (string, string, error) {
	var found, how string
	switch {
	case s.tsgoFlag != "":
		found, how = s.tsgoFlag, "-ts_tsgo"
	case os.Getenv("TSGO") != "":
		found, how = os.Getenv("TSGO"), "TSGO"
	case tsgoRlocationpath != "":
		p, err := runfiles.Rlocation(tsgoRlocationpath)
		if err != nil {
			return "", "", fmt.Errorf("the toolchain's tsgo is not in the runfiles (%v); pass -ts_tsgo=<path>", err)
		}
		found, how = p, "runfiles"
	default:
		return "", "", errNoTsgo
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", "", fmt.Errorf("tsgo from %s: %w", how, err)
	}
	return abs, how, nil
}

var tsSourceExtensions = []string{".ts", ".tsx", ".mts", ".cts"}

func (s *programStore) visit(rel string, files []string) {
	byDir := map[string][]string{rel: nil}
	for _, name := range files {
		file := path.Join(rel, name)
		dir := parentDir(file)
		byDir[dir] = append(byDir[dir], path.Base(file))
	}
	for dir, names := range byDir {
		s.files[dir] = names
		s.visited[dir] = nil
		if s.foreign[rel] != "" || s.foreign[dir] != "" {
			continue
		}
		for _, name := range names {
			if slices.Contains(tsSourceExtensions, path.Ext(name)) {
				s.visited[dir] = append(s.visited[dir], path.Join(dir, name))
			}
		}
	}
}

// Listed from Configure, before any directory generates: every package and
// every base an extends names is known when a rule asks about an ancestor.
func listTsConfigProgram(c *config.Config, rel string, tc *tsConfig, dirInfo func(string) (walk.DirInfo, error)) error {
	repoRoot := c.RepoRoot
	store := tc.programs
	cfg := tsconfigIn(rel)
	if m := tc.foreignManifest; m != "" {
		refused := foreignReason(m)
		store.record(&program{dir: rel, refused: refused})
		store.say("%s: not listed: %s", cfg, refused)
		store.sayForeign(m)
		return nil
	}
	resolved, configErr := store.resolveCompilerConfig(c, cfg, dirInfo)
	if errors.Is(configErr, tsconfig.ErrAdmission) {
		return configErr
	}
	if configErr != nil {
		log.Printf("typescript: %v", configErr)
	}
	store.readBases(repoRoot, rel, resolved)
	var refused string
	switch {
	case resolved == nil:
		refused = "the file could not be read"
	case !resolved.Inputs() && rel == "":
		refused = "neither include nor files in its extends chain, so tsgo would enumerate the whole repository"
	}
	if refused != "" {
		store.record(&program{dir: rel, refused: refused})
		store.say("%s: not listed: %s", cfg, refused)
		return nil
	}
	tsgo, err := store.binary()
	if errors.Is(err, errNoTsgo) {
		// Nothing reads the listing yet, so a run without the binary goes on.
		if !store.skipped {
			store.skipped = true
			log.Printf("typescript: programs are not listed: %v", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("typescript: %s: %w", cfg, err)
	}
	p, err := store.discoverCompilerProgram(c, cfg, tsgo, dirInfo)
	if err != nil {
		return err
	}
	store.record(p)
	if p.refused != "" {
		store.say("%s: not listed: %s", cfg, p.refused)
		return nil
	}
	// tsgo's diagnostics stay on the program and print under -ts_verbose only: a
	// types entry naming a generated file draws one on every run over a clean checkout.
	for _, d := range p.Diagnostics {
		store.say("%s: %s", cfg, d)
	}
	if store.packages[rel] == nil {
		store.say("%s: not a package: its listing names no first-party file", cfg)
		return nil
	}
	store.say("%s: %d files listed, %d roots, %d edges, %d type entries",
		cfg, len(p.Files), len(p.Roots), len(p.Edges), len(p.Types))
	return nil
}

func (s *programStore) requireAuthoredScope(c *config.Config, from, file string) {
	if err := s.authoredScopeError(c, from, file); err != nil {
		log.Fatal(err)
	}
}

func (s *programStore) authoredScopeError(c *config.Config, from, file string) error {
	scope := s.packageScope(c, parentDir(file))
	if scope == "" {
		return nil
	}
	switch s.metadataIdentity(c, scope) {
	case unknownInput:
		return s.incompleteOutput(scope)
	case generatedInput:
		return fmt.Errorf("typescript: %s imports %s with generated package scope %s: the authored module is checked at its source path, but the generated manifest is only available at its output path. Did you mean to keep package.json authored, or generate the module and manifest together in the same output layout?", from, file, scope)
	}
	return nil
}

// The visited .ts/.tsx/.mts/.cts files no listing names, once every directory
// has generated: one line per directory and a total.
func (s *programStore) reportUnlisted() {
	if !s.verbose || s.skipped {
		return
	}
	listed := map[string]bool{}
	for _, p := range s.programs {
		for _, f := range p.Files {
			listed[f] = true
		}
	}
	total, dirs := 0, 0
	for _, rel := range slices.Sorted(maps.Keys(s.visited)) {
		n := 0
		for _, f := range s.visited[rel] {
			if !listed[f] {
				n++
			}
		}
		if n == 0 {
			continue
		}
		total += n
		dirs++
		log.Printf("typescript: %s: %d file%s in no program", path.Join(".", rel), n, plural(n, "s"))
	}
	log.Printf("typescript: %d .ts/.tsx/.mts/.cts file%s in no program across %d director%s",
		total, plural(total, "s"), dirs, plural(dirs, "ies", "y"))
}

func plural(n int, many string, one ...string) string {
	if n == 1 {
		return strings.Join(one, "")
	}
	return many
}

// readBases records the tsconfig.json files rel's own extends names, the
// ts_config deps; a base of another name has no ts_config and is said.
func (s *programStore) readBases(repoRoot, rel string, resolved *tsconfig.Resolved) {
	if resolved == nil {
		return
	}
	dir := filepath.Join(repoRoot, filepath.FromSlash(rel))
	for _, spec := range resolved.Extends {
		basePath, ok := tsconfig.ResolveExtends(dir, spec)
		if !ok {
			continue
		}
		if st, err := os.Stat(basePath); err != nil || st.IsDir() {
			continue
		}
		baseRel, err := filepath.Rel(repoRoot, basePath)
		if err != nil || strings.HasPrefix(baseRel, "..") {
			continue
		}
		baseRel = filepath.ToSlash(baseRel)
		if path.Base(baseRel) != "tsconfig.json" {
			log.Printf("typescript: %s extends %q, a file Gazelle writes no "+
				"ts_config for (only a tsconfig.json is); declare that dep by "+
				"hand under # keep", tsconfigIn(rel), spec)
			continue
		}
		baseDir := parentDir(baseRel)
		if !slices.Contains(s.bases[rel], baseDir) {
			s.bases[rel] = append(s.bases[rel], baseDir)
		}
		s.extended[baseDir] = true
	}
}

func foreignReason(manifest string) string {
	return manifest + " is no importer in " + pnpmLockfileName
}

// Once per manifest, whatever the run finds under it.
func (s *programStore) sayForeign(manifest string) {
	if s.foreignSaid[manifest] {
		return
	}
	s.foreignSaid[manifest] = true
	log.Printf("typescript: %s, so pnpm installs nothing for it; nothing is "+
		"written under %s", foreignReason(manifest),
		orRepoRoot(parentDir(manifest)))
}

func listProgram(repoRoot, rel, tsgo string) (*program, error) {
	return listCompilerProgram(repoRoot, tsconfigIn(rel), tsgo, nil)
}

func (s *programStore) discoverCompilerProgram(c *config.Config, cfg, tsgo string, dirInfo func(string) (walk.DirInfo, error)) (*program, error) {
	p, err := s.compilerListing(c.RepoRoot, cfg, tsgo)
	if err != nil {
		return p, err
	}
	return s.reconcileCompilerProgram(c, p, dirInfo)
}

func (s *programStore) reconcileCompilerProgram(c *config.Config, p *program, dirInfo func(string) (walk.DirInfo, error)) (*program, error) {
	if p.manifest || p.refused != "" || p.config == "" {
		return p, nil
	}
	owners := map[string]bool{}
	var outputs []string
	// One crawl from every root's directory loads what a crawl per root would; per root it was quadratic.
	var dirs []string
	observed := map[string]bool{}
	for _, root := range p.Roots {
		if firstParty(root) && !observed[parentDir(root)] {
			observed[parentDir(root)] = true
			dirs = append(dirs, parentDir(root))
		}
	}
	if dirInfo != nil {
		s.observeBuilds(dirs, dirInfo)
	} else {
		s.observeBuilds(dirs)
	}
	for _, root := range p.Roots {
		if !firstParty(root) {
			continue
		}
		if s.generatedProgramFile(c, s.index, root, true) {
			outputs = append(outputs, root)
		}
		for dir := parentDir(root); ; dir = parentDir(dir) {
			owners[dir] = true
			if dir == "" {
				break
			}
		}
	}
	if len(outputs) == 0 {
		return p, nil
	}
	cfg := p.config
	dir := filepath.Join(c.RepoRoot, filepath.FromSlash(parentDir(cfg)))
	for _, pkg := range slices.Sorted(maps.Keys(owners)) {
		if file := s.emission.files[pkg]; file != nil {
			for _, stored := range file.Rules {
				r := s.semanticRule(emissionLabel(c.RepoName, pkg, ":"+stored.Name()))
				declared := ruleOutputs(r, c.RepoName, pkg)
				var files []string
				for _, output := range declared.files {
					info, err := os.Stat(filepath.Join(c.RepoRoot, filepath.FromSlash(path.Join(pkg, output))))
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						return nil, fmt.Errorf("%s: compiler discovery output %s: %w", cfg, path.Join(pkg, output), err)
					}
					if info.Mode().IsRegular() {
						files = append(files, output)
					}
				}
				if declared.tree != "" {
					files = append(files, declared.tree)
				}
				for _, output := range files {
					outputs = append(outputs, path.Join(pkg, output))
				}
			}
		}
	}
	for i, output := range outputs {
		file := filepath.Join(c.RepoRoot, filepath.FromSlash(output))
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(rel, "*?") {
			return nil, fmt.Errorf("%s: generated output %s cannot be excluded literally from compiler discovery; rename the output to remove wildcard characters", cfg, output)
		}
		outputs[i] = filepath.ToSlash(rel)
	}
	tsgo, err := s.binary()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(tsgo, "-p", cfg, "--showConfig", "--pretty", "false", "--locale", "en")
	cmd.Dir = c.RepoRoot
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: compiler discovery exclusions: %w\n%s", cfg, err, raw)
	}
	var effective struct {
		CompilerOptions json.RawMessage `json:"compilerOptions"`
		Exclude         []string        `json:"exclude"`
	}
	if err := json.Unmarshal(raw, &effective); err != nil {
		return nil, fmt.Errorf("%s: compiler discovery exclusions: %w", cfg, err)
	}
	if len(effective.CompilerOptions) == 0 {
		return nil, fmt.Errorf("%s: --showConfig did not return compilerOptions; select a compiler supporting effective discovery configuration", cfg)
	}
	slices.Sort(outputs)
	return listCompilerProgram(c.RepoRoot, cfg, tsgo, append(effective.Exclude, slices.Compact(outputs)...))
}

func listCompilerProgram(repoRoot, cfg, tsgo string, exclude []string, roots ...string) (*program, error) {
	return listCompilerProgramContext(context.Background(), repoRoot, cfg, tsgo, exclude, roots...)
}

// Held exclusively while a projection config exists, and shared by every prefetched
// listing, so no concurrent listing can read a projection.
var projectionConfigs sync.RWMutex

func listCompilerProgramContext(ctx context.Context, repoRoot, cfg, tsgo string, exclude []string, roots ...string) (*program, error) {
	project := cfg
	if len(roots) > 0 || len(exclude) > 0 {
		projectionConfigs.Lock()
		defer projectionConfigs.Unlock()
		dir := filepath.Join(repoRoot, filepath.FromSlash(parentDir(cfg)))
		files := make([]string, 0, len(roots))
		for _, root := range roots {
			rel, err := filepath.Rel(dir, filepath.Join(repoRoot, filepath.FromSlash(root)))
			if err != nil {
				return nil, err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		// The compiler rejects source arguments with -p; an adjacent config preserves path defaults.
		file, err := os.CreateTemp(dir, ".gazelle-tsconfig-*.json")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		projection := map[string]any{"extends": "./" + path.Base(cfg)}
		if len(roots) > 0 {
			projection["files"], projection["include"] = files, []string{}
		}
		if len(exclude) > 0 {
			projection["exclude"] = append(slices.Clone(exclude), path.Base(file.Name()))
		}
		err = json.NewEncoder(file).Encode(projection)
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		project = file.Name()
	}
	// --pretty false: a FORCE_COLOR in the environment would otherwise colour the diagnostics.
	p, err := runListingContext(ctx, repoRoot, tsgo, cfg, []string{"-p", project, "--noEmit", "--listFilesOnly", "--explainFiles",
		"--pretty", "false"})
	if err != nil {
		return nil, err
	}
	p.dir = parentDir(cfg)
	return p, nil
}

func (s *programStore) withKeptSources(c *config.Config, p *program, pkg string, dirInfo ...func(string) (walk.DirInfo, error)) *program {
	return s.withSources(c, p, s.keptProgramSources(c, pkg, dirInfo...))
}

func (s *programStore) withSources(c *config.Config, p *program, sources []string) *program {
	if p.refused != "" {
		return p
	}
	listedFiles := make(map[string]bool, len(p.Files))
	for _, file := range p.Files {
		listedFiles[file] = true
	}
	var missing []string
	for _, file := range sources {
		if programCandidate(file) && !listedFiles[file] && !s.generatedProgramFile(c, s.index, file, true) {
			missing = append(missing, file)
			listedFiles[file] = true
		}
	}
	if len(missing) == 0 {
		return p
	}
	tsgo, err := s.binary()
	if err != nil {
		log.Fatalf("typescript: %s: %v", p.config, err)
	}
	roots := append(slices.Clone(p.Roots), missing...)
	slices.Sort(roots)
	roots = slices.Compact(roots)
	var listed *program
	if p.manifest {
		listed, err = listManifestRoots(c.RepoRoot, p.config, tsgo, roots)
	} else {
		listed, err = listCompilerProgram(c.RepoRoot, p.config, tsgo, nil, roots...)
	}
	if err != nil {
		log.Fatalf("typescript: %v", err)
	}
	return listed
}

func (s *programStore) compilerProgram(c *config.Config, selectedConfig, pkg string,
	dirInfo ...func(string) (walk.DirInfo, error)) *program {
	cfg := compilerConfigSource(c, selectedConfig, pkg, dirInfo...)
	if cfg == "" || s.generatedFile(cfg, true) {
		return nil
	}
	var observe func(string) (walk.DirInfo, error)
	if len(dirInfo) > 0 {
		observe = dirInfo[0]
	}
	if _, err := s.resolveCompilerConfig(c, cfg, observe); errors.Is(err, tsconfig.ErrAdmission) {
		log.Fatal(err)
	}
	set := s.srcs(pkg, getConfig(c))
	sources := s.keptProgramSources(c, pkg, dirInfo...)
	for _, files := range [][]string{set.library, set.test, set.declaration} {
		sources = append(sources, files...)
	}
	if cfg == tsconfigIn(parentDir(cfg)) {
		p := s.inputs[parentDir(cfg)].program
		if p == nil {
			p = s.programs[parentDir(cfg)]
		}
		if p != nil {
			if p.manifest || p.refused != "" {
				return nil
			}
			return s.withSources(c, p, sources)
		}
	}
	p := s.compilerPrograms[cfg]
	if p == nil {
		if _, err := os.Stat(filepath.Join(c.RepoRoot, filepath.FromSlash(cfg))); os.IsNotExist(err) {
			return nil
		}
		tsgo, err := s.binary()
		if err != nil {
			log.Fatalf("typescript: %s: %v", cfg, err)
		}
		p, err = s.discoverCompilerProgram(c, cfg, tsgo, observe)
		if err != nil {
			log.Fatalf("typescript: %v", err)
		}
		if s.compilerPrograms == nil {
			s.compilerPrograms = map[string]*program{}
		}
		s.compilerPrograms[cfg] = p
	}
	return s.withSources(c, p, sources)
}

func compilerClosureError(p *program, consumer, selected string) error {
	if p != nil && p.refused == "" {
		return nil
	}
	reason := "no observed compiler program"
	if p != nil {
		reason = p.refused
	}
	return fmt.Errorf("typescript: %s cannot discover its compiler closure: selected tsconfig %q: %s. Did you mean to select an authored, readable compiler configuration, or keep the whole rule or ignore its package and maintain sources and deps manually?", consumer, selected, reason)
}

// A vitest config's listing: no tsconfig.json is its program (--ignoreConfig,
// else TS5112), a .mjs is a root (--allowJs), and the runner's resolution.
var vitestConfigFlags = []string{"--noEmit", "--listFilesOnly",
	"--explainFiles", "--ignoreConfig", "--allowJs", "--module", "esnext",
	"--moduleResolution", "bundler", "--skipLibCheck", "--pretty", "false"}

func listVitestConfig(repoRoot, tsgo, cfg string,
) (*program, error) {
	args := append(slices.Clone(vitestConfigFlags), cfg)
	return runListing(repoRoot, tsgo, cfg, args)
}

func (s *programStore) configProgram(repoRoot, cfg string) *program {
	if s.generatedFile(cfg, true) {
		return nil
	}
	s.requireAuthoredScope(s.repoConfig, cfg, cfg)
	if p := s.vitestPrograms[cfg]; p != nil {
		return p
	}
	if s.vitestPrograms == nil {
		s.vitestPrograms = map[string]*program{}
	}
	tsgo, err := s.binary()
	if err != nil {
		log.Fatalf("typescript: %s: %v", cfg, err)
	}
	p, err := listVitestConfig(repoRoot, tsgo, cfg)
	if err != nil {
		log.Fatalf("typescript: %v", err)
	}
	s.vitestPrograms[cfg] = p
	return p
}

// installMissing says why the listing cannot be trusted: a root lockfile with
// no node_modules from pnpm; tsgo prints no line for an unresolved import.
func installMissing(repoRoot string) string {
	if _, err := os.Stat(filepath.Join(repoRoot, pnpmLockfileName)); err != nil {
		return ""
	}
	marker := filepath.Join(repoRoot, "node_modules", ".modules.yaml")
	if _, err := os.Stat(marker); err == nil {
		return ""
	}
	return pnpmLockfileName + " is at the root and node_modules/.modules.yaml " +
		"is not: tsgo lists no edge into a package that is not installed, so " +
		"run pnpm install and rerun"
}

func (s *programStore) requireInstall(repoRoot string) {
	if s.installChecked {
		return
	}
	s.installChecked = true
	if why := installMissing(repoRoot); why != "" {
		log.Fatalf("typescript: %s", why)
	}
}

func (p *program) typeEdges() []explainfiles.Edge {
	cfg := p.config
	if cfg == "" {
		cfg = tsconfigIn(p.dir)
	}
	var out []explainfiles.Edge
	for _, entries := range [][]explainfiles.TypeEntry{p.Types, p.Implicit} {
		for _, te := range entries {
			out = append(out, explainfiles.Edge{Kind: explainfiles.TypeReference,
				From: cfg, To: te.File, Specifier: te.Entry})
		}
	}
	for _, edge := range p.Edges {
		if edge.From == cfg && edge.Kind == explainfiles.TypeReference {
			out = append(out, edge)
		}
	}
	return out
}

func (p *program) candidateEdge(candidate resolutionCandidate) explainfiles.Edge {
	selected := explainfiles.Edge{From: candidate.from, To: candidate.path, Specifier: candidate.specifier, Kind: candidate.kind}
	if !selected.Kind.ModuleSpecifier() {
		return selected
	}
	for _, edge := range p.Edges {
		if edge.From == candidate.from && edge.Specifier == candidate.specifier && edge.To == candidate.resolved && edge.Kind.ModuleSpecifier() {
			selected.Kind = edge.Kind
			if selected.Kind == explainfiles.Import {
				break
			}
		}
	}
	return selected
}

// tsgo exits non-zero on any diagnostic and still lists what it could, so the
// listing is kept whatever the exit; TS18003 alone is a program with no inputs.
func runListing(repoRoot, tsgo, subject string, args []string,
) (*program, error) {
	return runListingContext(context.Background(), repoRoot, tsgo, subject, args)
}

func runListingContext(ctx context.Context, repoRoot, tsgo, subject string, args []string,
) (*program, error) {
	args = append(append([]string{}, args...), "--traceResolution", "--locale", "en")
	cmd := exec.CommandContext(ctx, tsgo, args...)
	if ctx.Done() != nil {
		dieWithParent(cmd)
	}
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	listing, candidates, err := splitResolutionTrace(repoRoot, stdout.String())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	l, err := explainfiles.Parse(listing)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	p := &program{Listing: *l, candidates: candidates, config: subject}
	for i := range p.candidates {
		candidate := &p.candidates[i]
		if candidate.kind == explainfiles.TypeReference && (candidate.from == "" || path.Base(candidate.from) == "__inferred type names__.ts") {
			candidate.from = subject
		}
	}
	switch {
	case runErr == nil:
		return p, nil
	case len(p.Diagnostics) == 0:
		return nil, fmt.Errorf("%s: tsgo %s failed (%v) with no diagnostic:\n%s%s",
			subject, strings.Join(args, " "), runErr, stdout.String(),
			stderr.String())
	case len(p.Files) > 0, noInputs(p):
		return p, nil
	}
	p.refused = fmt.Sprintf("tsgo %v: %s", runErr,
		strings.Join(p.Diagnostics, "\n"))
	return p, nil
}

func noInputs(p *program) bool {
	for _, d := range p.Diagnostics {
		if !strings.Contains(d, "error TS18003:") {
			return false
		}
	}
	return len(p.Diagnostics) > 0
}

func listManifestProgram(c *config.Config, rel string, tc *tsConfig) {
	if tc.foreignManifest != "" || tc.programs.metadataIdentity(c, path.Join(rel, "package.json")) != authoredInput {
		return
	}
	repoRoot := c.RepoRoot
	roots, err := manifestProgramRoots(repoRoot, rel)
	if err != nil {
		log.Printf("typescript: %v; no manifest-owned program generated", err)
		return
	}
	if len(roots) == 0 {
		return
	}
	tsgo, err := tc.programs.binary()
	if errors.Is(err, errNoTsgo) {
		tc.programs.skipped = true
		return
	}
	if err != nil {
		log.Fatalf("typescript: %v", err)
	}
	p, err := listManifestRoots(repoRoot, path.Join(rel, "package.json"), tsgo, roots)
	if err != nil {
		log.Fatalf("typescript: %v", err)
	}
	tc.programs.record(p)
}

func listManifestRoots(repoRoot, manifest, tsgo string, roots []string) (*program, error) {
	args := []string{"--noEmit", "--listFilesOnly", "--explainFiles", "--ignoreConfig", "--allowJs", "--module", "preserve", "--types", "*", "--skipLibCheck", "--pretty", "false"}
	p, err := runListing(repoRoot, tsgo, manifest, append(args, roots...))
	if err != nil {
		return nil, err
	}
	p.dir = parentDir(manifest)
	p.manifest = true
	return p, nil
}
