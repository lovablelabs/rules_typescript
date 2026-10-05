package typescript

import (
	"errors"
	"io/fs"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"

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
		// Gazelle 0.47's Config.Clone leaves AliasMap shared.
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
		// Gazelle 0.47 mutates the shared parent map before this Configure boundary.
		// https://github.com/bazelbuild/bazel-gazelle/blob/v0.47.0/config/config.go
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
		// Gazelle 0.47 resolves a chain through the terminal mapping's FromKind.
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
	seenDirs := map[string]bool{}
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
			f := s.emission.files[dir]
			if len(dirInfo) > 0 {
				if info, err := dirInfo[0](dir); err == nil {
					if _, known := s.files[dir]; !known {
						s.visit(dir, info.RegularFiles)
					}
					if f == nil {
						f = info.File
					}
				}
			}
			if f == nil {
				continue
			}
			s.emission.setFile(dir, f)
			if s.inputs[dir].config == nil {
				s.inputs[dir] = programInput{config: s.buildConfig(dir)}
			}
			if s.emission.registeredRules(dir, f) {
				continue
			}
			for _, r := range f.Rules {
				key := emissionLabel(s.repoConfig.RepoName, dir, ":"+r.Name())
				if s.emission.rules[key] == nil {
					s.emission.setRule(key, r)
				}
			}
			s.emission.recordRegistered(dir, f)
		}
	}
	for _, pkg := range pkgs {
		load(pkg)
	}
	if len(dirInfo) == 0 {
		return
	}
	var queue []string
	for dir := range seenDirs {
		if file := s.emission.files[dir]; file != nil {
			for _, stored := range file.Rules {
				key := emissionLabel(s.repoConfig.RepoName, dir, ":"+stored.Name())
				if kind := canonicalRule(s.inputs[dir].config, stored).Kind(); kind == "ts_compile" || kind == "ts_test" || kind == "ts_binary" || kind == "ts_proto_library" {
					queue = append(queue, key)
				}
			}
		}
	}
	seenTargets := map[string]bool{}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if member := s.emission.lock.memberCompilerTarget(s.repoConfig.RepoName, key); member != "" {
			key = member
		}
		s.emission.crawlMemo()
		if seenTargets[key] || s.emission.crawled[key] {
			continue
		}
		seenTargets[key] = true
		target, err := parseLabel(key)
		if err != nil || target.Repo != "" {
			continue
		}
		load(target.Pkg)
		s.emission.crawlMemo()
		s.emission.crawledPkgs[target.Pkg] = true
		r := s.semanticRule(key)
		if r == nil {
			continue
		}
		s.emission.crawled[key] = true
		references, _ := runtimeDependencies(r)
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
		for _, text := range references {
			if text != "" {
				queue = append(queue, emissionLabel(s.repoConfig.RepoName, target.Pkg, text))
			}
		}
	}
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
	data, err := os.ReadFile(path)
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
	if st, err := os.Stat(candidate); err != nil || st.IsDir() {
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

// ---- Configurer implementation ---------------------------------------------

// configureTsConfig is tsLang.Configure for one directory: the parent's config
// cloned, the codegens declared here, the program listed.
func configureTsConfig(c *config.Config, rel string, f *rule.File, dirInfo func(string) (walk.DirInfo, error)) {
	tc := getConfig(c).clone()
	tc.programs.recordBuild(c, rel, f)

	if err := configureProto(c, rel, f, tc); err != nil {
		log.Fatalf("typescript: %v", err)
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
			log.Fatalf("typescript: %s: %v", pnpmLockfileName, err)
		}
	}

	tc.programs.recordEmissionConsumers(c, rel)
	tc.programs.emission.lock = tc.lock
	currentDir := filepath.Join(c.RepoRoot, rel)
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
	switch tc.programs.metadataIdentity(c, tsconfigIn(rel)) {
	case generatedInput, unknownInput:
		c.Exts[languageName] = tc
		return
	}
	if handWrittenTsConfigIn(currentDir, c.RepoRoot) != "" {
		err := listTsConfigProgram(c, rel, tc, dirInfo)
		input := tc.programs.inputs[rel]
		input.discoveryErr = err
		tc.programs.inputs[rel] = input
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
