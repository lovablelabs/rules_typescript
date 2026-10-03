package typescript

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/resolve"
)

type protoGraphNode struct {
	Label   string   `json:"label"`
	Sources []string `json:"sources"`
	Deps    []string `json:"deps"`
}

type protoGraph struct {
	Nodes   []protoGraphNode  `json:"nodes"`
	Roots   map[string]string `json:"roots"`
	nodes   map[string]*protoGraphNode
	sources map[string][]*protoGraphNode
	labels  map[string]label.Label
}

func loadProtoGraph(file string) (*protoGraph, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	g := &protoGraph{}
	if err := json.Unmarshal(b, g); err != nil {
		return nil, fmt.Errorf("typescript proto graph: %w", err)
	}
	g.nodes = map[string]*protoGraphNode{}
	g.sources = map[string][]*protoGraphNode{}
	g.labels = map[string]label.Label{}
	for i := range g.Nodes {
		node := &g.Nodes[i]
		l, err := parseLabel(node.Label)
		if err != nil || !l.Canonical || l.Repo == "" {
			return nil, fmt.Errorf("typescript proto graph: expected canonical external label, got %q", node.Label)
		}
		if _, exists := g.nodes[node.Label]; exists {
			return nil, fmt.Errorf("typescript proto graph: duplicate native owner %s", node.Label)
		}
		g.nodes[node.Label] = node
		g.labels[node.Label] = l
		for _, source := range node.Sources {
			g.sources[source] = append(g.sources[source], node)
		}
	}
	for _, node := range g.Nodes {
		for _, dep := range node.Deps {
			if g.nodes[dep] == nil {
				return nil, fmt.Errorf("typescript proto graph: %s depends on missing owner %s", node.Label, dep)
			}
		}
	}
	for spelling, native := range g.Roots {
		l, err := parseLabel(spelling)
		if err != nil {
			return nil, err
		}
		target, ok := g.labels[native]
		if !ok {
			return nil, fmt.Errorf("typescript proto graph: root %s has no native owner", spelling)
		}
		g.labels[l.String()] = target
	}
	return g, nil
}

func (g *protoGraph) normalize(l label.Label) label.Label {
	if g != nil {
		if target, ok := g.labels[l.String()]; ok {
			return target
		}
	}
	return l
}

func (g *protoGraph) providers(c *config.Config, imp string) []*protoGraphNode {
	if g == nil {
		return nil
	}
	if override, ok := resolve.FindRuleWithOverride(c, resolve.ImportSpec{Lang: "proto", Imp: imp}, "proto"); ok {
		normalized := g.normalize(override)
		if override.Repo != "" && override.Repo != c.RepoName && !normalized.Canonical {
			log.Fatalf("typescript proto graph: apparent override %s is not a declared graph root; use its canonical native label", override.String())
		}
		node := g.nodes[normalized.String()]
		if node == nil {
			return nil
		}
		var owners []*protoGraphNode
		for _, owner := range g.directOwners(node) {
			if slices.Contains(owner.Sources, imp) {
				owners = append(owners, owner)
			}
		}
		return owners
	}
	return g.sources[imp]
}

func (g *protoGraph) directOwners(node *protoGraphNode) []*protoGraphNode {
	seen := map[string]bool{}
	var result []*protoGraphNode
	var visit func(*protoGraphNode)
	visit = func(current *protoGraphNode) {
		if seen[current.Label] {
			return
		}
		seen[current.Label] = true
		if len(current.Sources) > 0 {
			result = append(result, current)
			return
		}
		for _, dep := range current.Deps {
			visit(g.nodes[dep])
		}
	}
	visit(node)
	return result
}

func (s *protoStore) needsWrapper(node *protoGraphNode, id *protoIdentity) bool {
	for _, source := range node.Sources {
		if s.runtimeImports[source] == "" || slices.Contains(id.Options, "bootstrap_wkt=true") {
			return true
		}
	}
	return false
}

func (s *protoStore) externalObservations(local []protoObservation) ([]protoObservation, error) {
	if s.graph == nil {
		return local, nil
	}
	result := slices.Clone(local)
	seen := map[string]bool{}
	localSources := map[string]bool{}
	for _, obs := range local {
		for _, source := range obs.paths {
			localSources[obs.identity.Name+":"+source] = true
		}
	}
	var add func(*protoGraphNode, protoObservation)
	add = func(node *protoGraphNode, from protoObservation) {
		for _, owner := range s.graph.directOwners(node) {
			key := from.identity.Name + ":" + owner.Label
			if seen[key] {
				continue
			}
			seen[key] = true
			if !s.needsWrapper(owner, from.identity) {
				continue
			}

			obs := protoObservation{config: from.config, native: s.graph.labels[owner.Label], identity: from.identity, paths: owner.Sources}
			for _, dep := range owner.Deps {
				for _, direct := range s.graph.directOwners(s.graph.nodes[dep]) {
					if !s.needsWrapper(direct, from.identity) {
						continue
					}

					obs.nativeDeps = append(obs.nativeDeps, s.graph.labels[direct.Label])
					add(direct, from)
				}
			}
			result = append(result, obs)
		}
	}
	for _, obs := range local {
		for _, imp := range obs.imports {
			_, overridden := resolve.FindRuleWithOverride(obs.config, resolve.ImportSpec{Lang: "proto", Imp: imp}, "proto")
			if (!overridden && localSources[obs.identity.Name+":"+imp]) || (s.runtimeImports[imp] != "" && !slices.Contains(obs.identity.Options, "bootstrap_wkt=true")) {
				continue
			}
			matches := s.graph.providers(obs.config, imp)
			if len(matches) == 0 {
				continue
			}
			if len(matches) != 1 {
				return nil, fmt.Errorf("typescript proto graph: %s has %d native owners; set a native proto resolve override", imp, len(matches))
			}
			add(matches[0], obs)
		}
	}
	return result, nil
}
