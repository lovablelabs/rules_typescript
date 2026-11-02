package npm_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

const nodeModules = "node_modules"

// resolveFrom is Node's walk from a package directory, node_modules/<name> at
// every ancestor, to the resolved package's realpath, or "".
func resolveFrom(from, name string) string {
	for dir := from; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, nodeModules, name)
		if filepath.Base(dir) == nodeModules {
			candidate = filepath.Join(dir, name)
		}
		if isPackage(candidate) {
			real, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return ""
			}
			return real
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

func isPackage(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "package.json"))
	return err == nil
}

// Every chain starts at a name the importer declares and follows each
// dependent's own edge, the link beside its store tree, as node does.
func TestEachDependentResolvesItsOwnVersion(t *testing.T) {
	tree := verify.New(t)

	for _, fixture := range []struct {
		importer string
		chains   [][2][]string
	}{
		{"multi_version", [][2][]string{
			// The importer declared minimatch@9, so that is what it links.
			{{"minimatch", "brace-expansion", "balanced-match"},
				{"9.0.9", "2.0.2", "1.0.2"}},
			// test-exclude's own edge is the newer major of all three.
			{{"test-exclude", "minimatch", "brace-expansion", "balanced-match"},
				{"7.0.2", "10.2.4", "5.0.4", "4.0.4"}},
			{{"test-exclude", "glob", "minimatch", "brace-expansion", "balanced-match"},
				{"7.0.2", "10.5.0", "9.0.9", "2.0.2", "1.0.2"}},
		}},
		// A scoped name, whose store directory is one path segment while the name
		// inside it is two.
		{"scoped_multi_version", [][2][]string{
			{{"@vitejs/plugin-react", "@rolldown/pluginutils"}, {"5.2.0", "1.0.0-rc.3"}},
			{{"@vitejs/plugin-react", "vite", "rolldown", "@rolldown/pluginutils"},
				{"5.2.0", "8.3.5", "1.2.16", "1.0.1"}},
		}},
	} {
		root := tree.FoundDir("*/" + fixture.importer + "/node_modules").Abs()
		for _, chain := range fixture.chains {
			followChain(t, fixture.importer, filepath.Dir(root), chain[0], chain[1])
		}
	}
}

func followChain(t *testing.T, importer, from string, names, versions []string,
) {
	t.Helper()
	dir := from
	for i, name := range names {
		resolved := resolveFrom(dir, name)
		if resolved == "" {
			t.Errorf("%s: %v does not resolve %q: nothing by that name up from %s",
				importer, names[:i], name, dir)
			return
		}
		if got := versionOf(t, resolved); got != versions[i] {
			t.Errorf("%s: %v resolves %q to %s@%s (at %s), want %s@%s",
				importer, names[:i], name, name, got, resolved, name, versions[i])
		}
		dir = resolved
	}
}

// The importer declares app-a's ansi-styles and reaches the other peer variant
// only through wrap-ansi: both are store trees and each edge names its own.
func TestEachDependentResolvesItsOwnPeerSet(t *testing.T) {
	tree := verify.New(t)
	root := tree.FoundDir("*/peer_variant_both/node_modules").Abs()

	for _, chain := range [][2][]string{
		{{"ansi-styles", "ansi-regex"}, {"6.2.3", "5.0.1"}},
		{{"wrap-ansi", "ansi-styles", "ansi-regex"}, {"8.1.0", "6.2.3", "6.2.2"}},
	} {
		followChain(t, "peer_variant_both", filepath.Dir(root), chain[0], chain[1])
	}

	// One tree per resolution, `<name>@<version>_<peer id>`; only the prefix is
	// pinned, since the peer id ends in a digest of the whole peer set.
	const pattern = "*/features/node_modules/.pnpm/ansi-styles@6.2.3_*/" +
		"node_modules/ansi-styles"
	if store := tree.Find(pattern); len(store) != 2 {
		t.Errorf("%d directories match %s, want 2: one tree per peer set",
			len(store), pattern)
	}
}
