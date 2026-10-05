package typescript

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/resolve"
)

// A package.json as the package model reads it: its name, and the names its
// dependencies and devDependencies declare.
type manifest struct {
	dir  string
	name string
	deps []string
}

// nearestManifest is the package.json at or above dir, the one tsc and node
// read for every file under dir.
func (s *programStore) nearestManifest(c *config.Config, dir string) *manifest {
	if s.index == nil || s.emission == nil {
		return s.findNearestManifest(c, dir)
	}
	memo := s.resolutionMemo()
	key := identityKey{c, dir}
	if m, ok := memo.manifests[key]; ok {
		return m
	}
	m := s.findNearestManifest(c, dir)
	memo.manifests[key] = m
	return m
}

func (s *programStore) findNearestManifest(c *config.Config, dir string) *manifest {
	for ; ; dir = parentDir(dir) {
		switch s.metadataIdentity(c, path.Join(dir, "package.json")) {
		case generatedInput, unknownInput:
			return nil
		case authoredInput:
			if m := s.readManifest(c.RepoRoot, dir); m != nil {
				return m
			}
		}
		if dir == "" {
			return nil
		}
	}
}

// Configure precedes the filtered walk; eligibility is checked when a File becomes an input.
func (s *programStore) metadataIdentity(c *config.Config, file string) inputIdentity {
	if s.index == nil || s.emission == nil {
		return s.resolveMetadataIdentity(c, file)
	}
	memo := s.resolutionMemo()
	key := identityKey{c, file}
	if identity, ok := memo.identities[key]; ok {
		return identity
	}
	identity := s.resolveMetadataIdentity(c, file)
	memo.identities[key] = identity
	return identity
}

// Answers that hold while the index, every rule list and every output declaration stay the same.
type resolutionMemo struct {
	stamp      identityStamp
	identities map[identityKey]inputIdentity
	generated  map[generatedKey]bool
	trees      map[string]treeOwner
	producers  map[string]fileProducer
	manifests  map[identityKey]*manifest
	// Per directory: its indexed codegen tree and the trees declared at or above it.
	indexedTrees  map[string]treeOwner
	ancestorTrees map[string]ancestorTrees
}

func (s *programStore) resolutionMemo() *resolutionMemo {
	stamp := identityStamp{s.index, s.emission.listEpoch, s.emission.outputsGeneration}
	if s.memo == nil || s.memo.stamp != stamp {
		s.memo = &resolutionMemo{stamp: stamp, identities: map[identityKey]inputIdentity{}, generated: map[generatedKey]bool{},
			trees: map[string]treeOwner{}, producers: map[string]fileProducer{}, manifests: map[identityKey]*manifest{},
			indexedTrees: map[string]treeOwner{}, ancestorTrees: map[string]ancestorTrees{}}
	}
	return s.memo
}

type identityKey struct {
	c    *config.Config
	file string
}

type identityStamp struct {
	index             *resolve.RuleIndex
	listEpoch         int
	outputsGeneration int
}

func (s *programStore) resolveMetadataIdentity(c *config.Config, file string) inputIdentity {
	if s.generatedProgramFile(c, s.index, file, true) {
		return generatedInput
	}
	if s.incompleteOutput(file) != nil {
		return unknownInput
	}
	if s.regularFile(filepath.Join(c.RepoRoot, filepath.FromSlash(file))) {
		return authoredInput
	}
	return unavailableInput
}

// Gazelle writes only BUILD files, after every query, so a run sees one source tree.
func (s *programStore) regularFile(file string) bool {
	if regular, ok := s.regularFiles[file]; ok {
		return regular
	}
	info, err := os.Stat(file)
	regular := err == nil && info.Mode().IsRegular()
	if s.regularFiles == nil {
		s.regularFiles = map[string]bool{}
	}
	s.regularFiles[file] = regular
	return regular
}

func (s *programStore) readManifest(repoRoot, dir string) *manifest {
	key := filepath.Join(repoRoot, filepath.FromSlash(dir))
	if m, ok := s.manifests[key]; ok {
		return m
	}
	m := readManifest(repoRoot, dir)
	if s.manifests == nil {
		s.manifests = map[string]*manifest{}
	}
	s.manifests[key] = m
	return m
}

func incompleteOutputConflict(pkg, name, attr string) error {
	return fmt.Errorf("typescript: cannot determine output provenance: //%s:%s has nonliteral %s. Did you mean to use literal output declarations for TypeScript discovery and inputs?", pkg, name, attr)
}

func (s *programStore) incompleteOutput(file string) error {
	if s.emission == nil || !firstParty(file) {
		return nil
	}
	for dir := parentDir(file); ; dir = parentDir(dir) {
		if f := s.emission.files[dir]; f != nil {
			if outputs := s.packageOutputs(dir, f); outputs != nil {
				if outputs.incomplete >= 0 {
					r := outputs.producers[outputs.incomplete]
					return incompleteOutputConflict(dir, r.Name(), ruleOutputs(r, s.repoConfig.RepoName, dir).incomplete)
				}
				return nil
			}
			for _, stored := range f.Rules {
				r := s.semanticRule(emissionLabel(s.repoConfig.RepoName, dir, ":"+stored.Name()))
				if r != nil {
					if attr := ruleOutputs(r, s.repoConfig.RepoName, dir).incomplete; attr != "" {
						return incompleteOutputConflict(dir, r.Name(), attr)
					}
				}
			}
			// Bazel output declarations cannot cross a subpackage's BUILD boundary.
			return nil
		}
		if dir == "" {
			return nil
		}
	}
}

func (s *programStore) newPackageTargetConflict(pkg string) error {
	if s.emission == nil || pkg == "" {
		return nil
	}
	for dir := parentDir(pkg); ; dir = parentDir(dir) {
		if f := s.emission.files[dir]; f != nil {
			for _, stored := range f.Rules {
				producer := emissionLabel(s.repoConfig.RepoName, dir, ":"+stored.Name())
				r := s.semanticRule(producer)
				if r == nil {
					continue
				}
				outputs := ruleOutputs(r, s.repoConfig.RepoName, dir)
				targets := append([]string{r.Name()}, outputs.files...)
				for _, target := range targets {
					if dirIsAncestorOf(pkg, path.Join(dir, target)) {
						return fmt.Errorf("typescript: cannot create package %s: target %s declared by %s would cross its BUILD boundary. Did you mean to retain the existing package boundary or move the declaring rule explicitly before generating this package?", pkg, emissionLabel(s.repoConfig.RepoName, dir, ":"+target), producer)
					}
				}
				if outputs.incomplete != "" {
					return incompleteOutputConflict(dir, r.Name(), outputs.incomplete)
				}
			}
			if f.Content != nil {
				return nil
			}
		}
		if dir == "" {
			return nil
		}
	}
}

func generatedDiscoveryConflict(file string) error {
	return fmt.Errorf("typescript: cannot discover program membership from generated metadata %s; checkout copies are not discovery inputs. Did you mean to use authored metadata for automatic discovery, or select a generated config under a different name on an explicitly kept compiler rule?", file)
}

func (s *programStore) packageScope(c *config.Config, dir string) string {
	for ; ; dir = parentDir(dir) {
		file := path.Join(dir, "package.json")
		if s.metadataIdentity(c, file) != unavailableInput {
			return file
		}
		if dir == "" {
			return ""
		}
	}
}

func readManifest(repoRoot, dir string) *manifest {
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(dir),
		"package.json"))
	if err != nil {
		return nil
	}
	var raw struct {
		Name            string            `json:"name"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	deps := map[string]bool{}
	for name := range raw.Dependencies {
		deps[name] = true
	}
	for name := range raw.DevDependencies {
		deps[name] = true
	}
	return &manifest{dir: dir, name: raw.Name,
		deps: slices.Sorted(maps.Keys(deps))}
}

func manifestProgramRoots(repoRoot, dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(dir), "package.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	roots := map[string]bool{}
	var visit func(any) error
	visit = func(value any) error {
		switch v := value.(type) {
		case string:
			if !programCandidate(v) {
				return nil
			}
			if strings.Contains(v, "*") || path.IsAbs(v) || strings.HasPrefix(path.Clean(v), "../") {
				return fmt.Errorf("%s/package.json: source export %q needs an explicit tsconfig.json program; did you mean to declare the package's compiler inputs there?", dir, v)
			}
			file := path.Join(dir, v)
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(file))); os.IsNotExist(err) {
				return nil
			} else if err != nil {
				return err
			}
			roots[file] = true
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(v)) {
				if err := visit(v[key]); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range v {
				if err := visit(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, field := range []string{"exports", "types", "typings", "main", "module", "browser"} {
		if err := visit(fields[field]); err != nil {
			return nil, err
		}
	}
	return slices.Sorted(maps.Keys(roots)), nil
}
