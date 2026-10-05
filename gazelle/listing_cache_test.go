package typescript

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/mikn/rules_typescript/gazelle/incremental"
)

// A reused listing is only sound while nothing it read, or could have globbed, changed.
func TestListingCache_ReusesUntilAnInputItReadChanges(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]}}`,
		"app/a.ts":          "import { b } from './b';\nexport const a = b;\n",
		"app/b.ts":          "export const b = 1;\n",
	})
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	listingCacheDir = t.TempDir()
	runs := 0
	uncachedListing = func(ctx context.Context, repoRoot, tsgo, subject string, args []string) (*program, error) {
		runs++
		return runListingUncached(ctx, repoRoot, tsgo, subject, args)
	}
	t.Cleanup(func() { listingCacheDir, uncachedListing = "", runListingUncached })
	list := func() *program {
		listingNames, listingStats = newListingNames(), &incremental.StatCache{}
		sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
		p, err := listCompilerProgram(root, "app/tsconfig.json", tsgo, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	list()
	if p := list(); runs != 1 || !slices.Contains(p.Files, "app/b.ts") {
		t.Fatalf("an unchanged tree re-ran tsgo (%d runs) or lost a file: %q", runs, p.Files)
	}
	if err := os.WriteFile(filepath.Join(root, "app/c.ts"), []byte("export const c = 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := list(); runs != 2 || !slices.Contains(p.Files, "app/c.ts") {
		t.Fatalf("a file the include glob now matches was not listed (%d runs): %q", runs, p.Files)
	}
	if err := os.WriteFile(filepath.Join(root, "app/b.ts"), []byte("export { c as b } from './c';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if list(); runs != 3 {
		t.Fatalf("an edited listed file reused the listing (%d runs)", runs)
	}
}

// A patched listing must equal the listing tsgo would print for the edited tree.
func TestListingCache_PatchedImportEqualsAFreshListing(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[]}}`,
		"app/a.ts":          "import { b } from './b';\nexport const a = b;\n",
		"app/b.ts":          "export const b = 1;\n",
		"app/c.ts":          "import { d } from './d';\nexport const c = d;\n",
		"app/d.ts":          "export const d = 2;\n",
	})
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	listingCacheDir = t.TempDir()
	runs := 0
	uncachedListing = func(ctx context.Context, repoRoot, tsgo, subject string, args []string) (*program, error) {
		runs++
		return runListingUncached(ctx, repoRoot, tsgo, subject, args)
	}
	t.Cleanup(func() { listingCacheDir, uncachedListing = "", runListingUncached })
	list := func() *program {
		listingNames, listingStats = newListingNames(), &incremental.StatCache{}
		sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
		p, err := listCompilerProgram(root, "app/tsconfig.json", tsgo, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	list()
	if err := os.WriteFile(filepath.Join(root, "app/a.ts"), []byte("import { b } from './b';\nimport { d } from './d';\nexport const a = b + d;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patched := list()
	if runs != 1 {
		t.Fatalf("an import resolved from the same directory re-ran tsgo (%d runs): %v", runs, lastPatchRefusal.Load())
	}
	fresh, err := runListingUncached(context.Background(), root, tsgo, "app/tsconfig.json", []string{"-p", "app/tsconfig.json", "--noEmit", "--listFilesOnly", "--explainFiles", "--pretty", "false"})
	if err != nil {
		t.Fatal(err)
	}
	edges := func(p *program) []string {
		var out []string
		for _, e := range p.Edges {
			out = append(out, e.From+" -> "+e.To+" "+e.Specifier)
		}
		slices.Sort(out)
		return out
	}
	files := func(p *program) []string { f := slices.Clone(p.Files); slices.Sort(f); return f }
	if !slices.Equal(files(patched), files(fresh)) || !slices.Equal(edges(patched), edges(fresh)) {
		t.Errorf("patched listing differs from tsgo's\npatched %q\n%q\nfresh %q\n%q", files(patched), edges(patched), files(fresh), edges(fresh))
	}
	if err := os.WriteFile(filepath.Join(root, "app/a.ts"), []byte("import { b } from './b';\nimport { c } from './c';\nexport const a = b + c;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if list(); runs != 2 {
		t.Fatalf("an import with no precedent in its directory was patched (%d runs)", runs)
	}
}
