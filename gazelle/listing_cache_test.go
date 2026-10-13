package typescript

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mikn/rules_typescript/gazelle/incremental"
	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
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
	if p := list(); !slices.Contains(p.Files, "app/c.ts") {
		t.Fatalf("a file the include glob now matches was not listed (%d runs): %q", runs, p.Files)
	}
	if err := os.WriteFile(filepath.Join(root, "app/b.ts"), []byte("export { c as b } from './c';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := list(); !slices.ContainsFunc(p.Edges, func(e explainfiles.Edge) bool { return e.From == "app/b.ts" && e.To == "app/c.ts" }) {
		t.Fatalf("an edited listed file kept its old imports (%d runs): %v", runs, p.Edges)
	}
	before := runs
	if err := os.WriteFile(filepath.Join(root, "app/tsconfig.json"), []byte(`{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"strict":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if list(); runs != before+1 {
		t.Fatalf("an edited tsconfig.json reused the listing (%d runs)", runs)
	}
}

// listingRuns lists one program through the cache with fresh per-run state,
// and counts how often tsgo ran.
type listingRuns struct {
	t          *testing.T
	root, tsgo string
	runs       int
}

func newListingRuns(t *testing.T, files map[string]string) *listingRuns {
	t.Helper()
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, files)
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	l := &listingRuns{t: t, root: root, tsgo: tsgo}
	listingCacheDir = t.TempDir()
	uncachedListing = func(ctx context.Context, repoRoot, tsgo, subject string, args []string) (*program, error) {
		l.runs++
		return runListingUncached(ctx, repoRoot, tsgo, subject, args)
	}
	t.Cleanup(func() { listingCacheDir, uncachedListing = "", runListingUncached })
	return l
}

func (l *listingRuns) list(cfg string) *program {
	l.t.Helper()
	listingNames, listingStats = newListingNames(), &incremental.StatCache{}
	sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
	p, err := listCompilerProgram(l.root, cfg, l.tsgo, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	pendingWrites.Wait()
	return p
}

func (l *listingRuns) fresh(cfg string) *program {
	l.t.Helper()
	p, err := runListingUncached(context.Background(), l.root, l.tsgo, cfg, []string{"-p", cfg, "--noEmit", "--listFilesOnly", "--explainFiles", "--pretty", "false"})
	if err != nil {
		l.t.Fatal(err)
	}
	return p
}

const patchFixtureConfig = `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler","jsx":"react-jsx","types":[]},"include":["src/**/*"]}`

func patchFixture() map[string]string {
	return map[string]string{
		"app/package.json":                        `{"name":"app"}`,
		"app/tsconfig.json":                       patchFixtureConfig,
		"app/node_modules/react/package.json":     `{"name":"react","version":"1.0.0","types":"index.d.ts","exports":{".":{"types":"./index.d.ts"},"./jsx-runtime":{"types":"./jsx-runtime.d.ts"}}}`,
		"app/node_modules/react/index.d.ts":       "export declare const useState: number;\n",
		"app/node_modules/react/jsx-runtime.d.ts": "export declare const jsx: any;\nexport declare namespace JSX { interface IntrinsicElements { [k: string]: any } }\n",
		"app/src/a.ts":                            "import { b } from './b';\nexport const a = b;\n",
		"app/src/b.ts":                            "export const b = 1;\n",
		"app/src/c.ts":                            "import { d } from './lib/d.js';\nexport const c = d;\n",
		"app/src/lib/d.ts":                        "export const d = 2;\n",
		"app/src/lib/index.ts":                    "export * from './d';\n",
		"app/src/view/v.tsx":                      "import { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		"app/src/view/w.tsx":                      "import { a } from '../a';\nexport const W = () => <div>{a}</div>;\n",
		"app/tool.mjs":                            "export const tool = 1;\n",
	}
}

// A patched listing must be the listing tsgo prints for the edited tree, and
// each of these edits used to send the whole program back to tsgo.
func TestListingCache_PatchedListingEqualsAFreshListing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  map[string]string
		drop  []string
		check func(*program) bool
	}{
		{name: "an import with no precedent in its directory", edit: map[string]string{
			"app/src/a.ts": "import { b } from './b';\nimport { c } from './c';\nexport const a = b + c;\n",
		}},
		{name: "an import of a directory's index", edit: map[string]string{
			"app/src/b.ts": "import { d } from './lib';\nexport const b = d;\n",
		}},
		{name: "an import that resolves nowhere", edit: map[string]string{
			"app/src/b.ts": "import { gone } from './gone';\nexport const b = gone;\n",
		}},
		{name: "an added file", edit: map[string]string{
			"app/src/e.ts": "import { b } from './b';\nexport const e = b;\n",
		}},
		{name: "an added file in a new directory importing a package another directory imports", edit: map[string]string{
			"app/src/panel/deep/p.tsx": "import { useState } from 'react';\nimport { W } from '../../view/w';\nexport const P = () => <W>{useState}</W>;\n",
		}},
		{name: "a deleted file", drop: []string{"app/src/lib/index.ts"}},
		{name: "a deleted import target and its import", drop: []string{"app/src/b.ts"}, edit: map[string]string{
			"app/src/a.ts": "export const a = 1;\n",
		}},
		{name: "an added file an import now finds before the index", edit: map[string]string{
			"app/src/b.ts":   "import { d } from './lib';\nexport const b = d;\n",
			"app/src/lib.ts": "export const d = 3;\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListingRuns(t, patchFixture())
			l.list("app/tsconfig.json")
			writeWorkspace(t, l.root, tc.edit)
			for _, rel := range tc.drop {
				if err := os.Remove(filepath.Join(l.root, rel)); err != nil {
					t.Fatal(err)
				}
			}
			patched := l.list("app/tsconfig.json")
			if l.runs != 1 {
				t.Fatalf("the edit listed the program again (%d runs): %v", l.runs, lastPatchRefusal.Load())
			}
			if diff := listingDifference(patched, l.fresh("app/tsconfig.json")); diff != "" {
				t.Errorf("the patched listing differs from tsgo's:\n%s", diff)
			}
			if again := l.list("app/tsconfig.json"); l.runs != 1 || listingDifference(again, patched) != "" {
				t.Errorf("the patched entry was not kept for the next run (%d runs)", l.runs)
			}
		})
	}
}

// What the patch does not model goes back to tsgo rather than guessing.
func TestListingCache_PatchRefusesWhatItDoesNotModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit map[string]string
	}{
		{name: "a bare specifier no importer resolves", edit: map[string]string{
			"app/src/a.ts": "import { b } from './b';\nimport { z } from 'zod';\nexport const a = b + z;\n",
		}},
		{name: "an added file with a reference directive", edit: map[string]string{
			"app/src/e.ts": "/// <reference types=\"react\" />\nexport const e = 1;\n",
		}},
		{name: "a changed tsconfig", edit: map[string]string{
			"app/tsconfig.json": strings.Replace(patchFixtureConfig, `"types":[]`, `"types":[],"strict":true`, 1),
		}},
		{name: "an added file beside a declaration of the same name", edit: map[string]string{
			"app/src/b.d.ts": "export declare const b: number;\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListingRuns(t, patchFixture())
			l.list("app/tsconfig.json")
			writeWorkspace(t, l.root, tc.edit)
			got := l.list("app/tsconfig.json")
			if l.runs != 2 {
				t.Fatalf("the edit was patched instead of listed again (%d runs)", l.runs)
			}
			if diff := listingDifference(got, l.fresh("app/tsconfig.json")); diff != "" {
				t.Errorf("the relisted program differs from tsgo's:\n%s", diff)
			}
		})
	}
}

// Over every converge fixture and mutation, a patched listing equals tsgo's, and
// the Go resolver probes as tsgo traced each relative specifier.
func TestListingCache_PatchMatchesTsgoOnTheFixtureCorpus(t *testing.T) {
	requireTsgo(t)
	patched, relative := 0, 0
	for _, tc := range convergeCases() {
		for _, mut := range tc.mutations {
			t.Run(tc.name+"/"+mut.kind, func(t *testing.T) {
				l := newListingRuns(t, tc.files)
				configs := corpusConfigs(t, l.root)
				for _, cfg := range configs {
					relative += checkRelativeResolutions(t, l.root, l.list(cfg))
				}
				applyMutation(t, l.root, mut)
				for _, cfg := range configs {
					if _, err := os.Stat(filepath.Join(l.root, cfg)); err != nil {
						continue
					}
					before := listingPatched.Load()
					got := l.list(cfg)
					if listingPatched.Load() == before {
						continue
					}
					patched++
					if diff := listingDifference(got, l.fresh(cfg)); diff != "" {
						t.Errorf("%s: the patched listing differs from tsgo's:\n%s", cfg, diff)
					}
				}
			})
		}
	}
	t.Logf("%d patched listings, %d relative resolutions compared", patched, relative)
	if patched == 0 || relative == 0 {
		t.Errorf("the corpus exercised %d patched listings and %d relative resolutions", patched, relative)
	}
}

func corpusConfigs(t *testing.T, root string) []string {
	t.Helper()
	var configs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if d.Name() != "tsconfig.json" {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		// Gazelle lists every tsconfig.json but a root one that would enumerate the repository.
		if r, err := tsconfig.Resolve(p); err == nil && (r.Inputs() || rel != "tsconfig.json") {
			configs = append(configs, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return configs
}

// checkRelativeResolutions resolves each relative specifier p traced and
// requires tsgo's candidates; it returns how many it compared.
func checkRelativeResolutions(t *testing.T, root string, p *program) int {
	t.Helper()
	if len(p.resolution.Kinds) != 1 || len(p.resolution.RelativeTargets) != 1 {
		return 0
	}
	blocks := map[int][]resolutionCandidate{}
	for _, c := range p.candidates {
		if c.kind != explainfiles.TypeReference && relativeSpecifier(c.specifier) {
			blocks[c.block] = append(blocks[c.block], c)
		}
	}
	tp := &patcher{root: root, entry: &cachedListing{Resolution: p.resolution, Config: &patchConfig{}}}
	for id, want := range blocks {
		r := relativeResolver{t: tp, targets: p.resolution.RelativeTargets[0]}
		trace, _, ok := r.resolve(want[0].from, want[0].specifier)
		if !ok {
			t.Errorf("%s %q: refused: %s", want[0].from, want[0].specifier, tp.refusal)
			continue
		}
		_, got, err := splitResolutionTrace(root, trace)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			got[i].block = id
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s %q: the Go resolver probed\n  %v\ntsgo probed\n  %v", want[0].from, want[0].specifier, got, want)
		}
	}
	return len(blocks)
}

// project lists cfg's program with roots as its files list, through the cache.
func (l *listingRuns) project(cfg string, roots []string) *program {
	l.t.Helper()
	listingNames, listingStats = newListingNames(), &incremental.StatCache{}
	sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
	p, err := listCompilerProgram(l.root, cfg, l.tsgo, nil, roots...)
	if err != nil {
		l.t.Fatal(err)
	}
	pendingWrites.Wait()
	return p
}

func (l *listingRuns) freshProject(cfg string, roots []string) *program {
	l.t.Helper()
	dir, runs := listingCacheDir, l.runs
	listingCacheDir = ""
	defer func() { listingCacheDir, l.runs = dir, runs }()
	p, err := listCompilerProgram(l.root, cfg, l.tsgo, nil, roots...)
	if err != nil {
		l.t.Fatal(err)
	}
	return p
}

var projectionRoots = []string{"app/src/a.ts", "app/src/c.ts", "app/src/view/v.tsx"}

// A files-list projection was keyed on its list, so every root gained or lost
// listed the whole program again; its listing is now patched from its base's.
func TestListingCache_FilesListProjectionPatchesRootChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		base, roots []string
		edit        map[string]string
		drop        []string
	}{
		{name: "a gained root new to the program", roots: append(slices.Clone(projectionRoots), "app/src/view/w.tsx")},
		{name: "a gained root the program already imports", roots: append(slices.Clone(projectionRoots), "app/src/b.ts")},
		{name: "a gained root that is a new file", edit: map[string]string{
			"app/src/e.ts": "import { d } from './lib/d';\nexport const e = d;\n",
		}, roots: append(slices.Clone(projectionRoots), "app/src/e.ts")},
		{name: "a lost root nothing else imports", roots: []string{"app/src/a.ts", "app/src/c.ts"}},
		{name: "a lost root another root still imports", edit: map[string]string{
			"app/src/c.ts": "import { d } from './lib/d.js';\nimport { a } from './a';\nexport const c = d + a;\n",
		}, roots: []string{"app/src/c.ts", "app/src/view/v.tsx"}},
		{name: "a lost root whose file is deleted", drop: []string{"app/src/c.ts"}, roots: []string{"app/src/a.ts", "app/src/view/v.tsx"}},
		{name: "a gained and a lost root", roots: []string{"app/src/a.ts", "app/src/lib/index.ts", "app/src/view/v.tsx"}},
		{name: "a gained root beside a script tsgo does not root",
			base:  append(slices.Clone(projectionRoots), "app/tool.mjs"),
			roots: append(slices.Clone(projectionRoots), "app/src/view/w.tsx", "app/tool.mjs")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == nil {
				base = projectionRoots
			}
			l := newListingRuns(t, patchFixture())
			l.project("app/tsconfig.json", base)
			writeWorkspace(t, l.root, tc.edit)
			for _, rel := range tc.drop {
				if err := os.Remove(filepath.Join(l.root, rel)); err != nil {
					t.Fatal(err)
				}
			}
			patched := l.project("app/tsconfig.json", tc.roots)
			if l.runs != 1 {
				t.Fatalf("the files list change listed the program again (%d runs): %v", l.runs, lastPatchRefusal.Load())
			}
			if diff := listingDifference(patched, l.freshProject("app/tsconfig.json", tc.roots)); diff != "" {
				t.Errorf("the patched listing differs from tsgo's:\n%s", diff)
			}
			if again := l.project("app/tsconfig.json", tc.roots); l.runs != 1 || listingDifference(again, patched) != "" {
				t.Errorf("the patched entry was not kept under its own files list (%d runs)", l.runs)
			}
			if len(tc.edit)+len(tc.drop) > 0 {
				return
			}
			if first := l.project("app/tsconfig.json", base); l.runs != 1 || !slices.Equal(slices.Sorted(slices.Values(first.Roots)), projectionRoots) {
				t.Errorf("the first files list lost its own entry (%d runs): %q", l.runs, first.Roots)
			}
		})
	}
}

// A files list change the patch does not model goes back to tsgo.
func TestListingCache_FilesListProjectionRefusesWhatItDoesNotModel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		base, roots []string
		edit        map[string]string
	}{
		{name: "a gained root that does not exist", roots: append(slices.Clone(projectionRoots), "app/src/gone.ts")},
		{name: "a gained root with a reference directive", edit: map[string]string{
			"app/src/e.ts": "/// <reference types=\"react\" />\nexport const e = 1;\n",
		}, roots: append(slices.Clone(projectionRoots), "app/src/e.ts")},
		{name: "a gained JSON root", roots: append(slices.Clone(projectionRoots), "app/package.json")},
		{name: "a changed base config", edit: map[string]string{
			"app/tsconfig.json": strings.Replace(patchFixtureConfig, `"types":[]`, `"types":[],"strict":true`, 1),
		}, roots: append(slices.Clone(projectionRoots), "app/src/b.ts")},
		{name: "a lost script tsgo did not root", base: append(slices.Clone(projectionRoots), "app/tool.mjs"), roots: projectionRoots},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListingRuns(t, patchFixture())
			base := tc.base
			if base == nil {
				base = projectionRoots
			}
			l.project("app/tsconfig.json", base)
			writeWorkspace(t, l.root, tc.edit)
			got := l.project("app/tsconfig.json", tc.roots)
			if l.runs != 2 {
				t.Fatalf("the change was patched instead of listed again (%d runs)", l.runs)
			}
			if diff := listingDifference(got, l.freshProject("app/tsconfig.json", tc.roots)); diff != "" {
				t.Errorf("the relisted program differs from tsgo's:\n%s", diff)
			}
		})
	}
}

// Over every converge fixture config, a files-list projection that gains or
// loses a root is patched to the listing tsgo prints for it.
func TestListingCache_FilesListPatchMatchesTsgoOnTheFixtureCorpus(t *testing.T) {
	requireTsgo(t)
	patched := 0
	for _, tc := range convergeCases() {
		t.Run(tc.name, func(t *testing.T) {
			l := newListingRuns(t, tc.files)
			for _, cfg := range corpusConfigs(t, l.root) {
				base := l.list(cfg)
				var roots []string
				for _, r := range base.Roots {
					if firstParty(r) && rootStem(r) != "" {
						roots = append(roots, r)
					}
				}
				if len(roots) < 2 {
					continue
				}
				extra := projectionCandidates(t, l.root, parentDir(cfg), base.Files)
				variants := [][]string{roots[1:], roots[:len(roots)-1]}
				for _, f := range extra[:min(2, len(extra))] {
					variants = append(variants, append(slices.Clone(roots), f), append(slices.Clone(roots[1:]), f))
				}
				l.project(cfg, roots)
				for _, variant := range variants {
					before := listingPatched.Load()
					got := l.project(cfg, variant)
					if listingPatched.Load() == before {
						continue
					}
					patched++
					if diff := listingDifference(got, l.freshProject(cfg, variant)); diff != "" {
						t.Errorf("%s %q: the patched listing differs from tsgo's:\n%s", cfg, variant, diff)
					}
				}
			}
		})
	}
	t.Logf("%d patched files-list listings", patched)
	if patched == 0 {
		t.Error("the corpus exercised no patched files-list listing")
	}
}

// projectionCandidates are the TypeScript sources under dir the listing does not hold.
func projectionCandidates(t *testing.T, root, dir string, listed []string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !d.IsDir() && patchableSource(rel) && rootStem(rel) != "" && !slices.Contains(listed, rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// -ts_listing_patch_verify must check a listing patched to a new files list, not
// only one patched under its own key.
func TestListingCache_PatchVerifyListsAPatchedFilesList(t *testing.T) {
	l := newListingRuns(t, patchFixture())
	l.project("app/tsconfig.json", projectionRoots)
	listingPatchVerify = true
	t.Cleanup(func() { listingPatchVerify = false })
	before := listingPatched.Load()
	l.project("app/tsconfig.json", append(slices.Clone(projectionRoots), "app/src/view/w.tsx"))
	if listingPatched.Load() == before || l.runs != 2 {
		t.Fatalf("the patched files list was not listed by tsgo too (%d patched, %d runs)", listingPatched.Load()-before, l.runs)
	}
}

// The family entry fewest files apart can be one the patch must refuse, a
// script tsgo does not root among them; another entry of the family patches.
func TestListingCache_FilesListPatchesFromAnotherFamilyEntry(t *testing.T) {
	l := newListingRuns(t, patchFixture())
	l.project("app/tsconfig.json", projectionRoots)
	l.project("app/tsconfig.json", append(slices.Clone(projectionRoots), "app/src/b.ts", "app/src/lib/index.ts", "app/tool.mjs"))
	roots := append(slices.Clone(projectionRoots), "app/src/view/w.tsx", "app/tool.mjs")
	patched := l.project("app/tsconfig.json", roots)
	if l.runs != 2 {
		t.Fatalf("no family entry patched the files list (%d runs): %v", l.runs, lastPatchRefusal.Load())
	}
	if diff := listingDifference(patched, l.freshProject("app/tsconfig.json", roots)); diff != "" {
		t.Errorf("the patched listing differs from tsgo's:\n%s", diff)
	}
}

// A walk up from a rooted importer looped on path.Dir("/") == "/".
func TestListingPatch_ScopeWalksEndForRootedAndDottedImporters(t *testing.T) {
	for _, tc := range []struct {
		importer, scope string
		walk            []string
	}{
		{"/a/b.ts", "/", []string{"/a/package.json", "/package.json"}},
		{"/b.ts", "/", []string{"/package.json"}},
		{"./a/b.ts", "", []string{"a/package.json", "package.json"}},
		{"./b.ts", "", []string{"package.json"}},
		{"../x/b.ts", "", []string{"../x/package.json", "../package.json", "package.json"}},
		{"a/b.ts", "", []string{"a/package.json", "package.json"}},
	} {
		t.Run(tc.importer, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo")
			writeFile(t, filepath.Join(root, "package.json"), "{}")
			p := &patcher{root: root, scopes: map[string]string{}, nms: map[string]string{}}
			scope := p.nearest(p.scopes, parentDir(tc.importer), "package.json", func(i fs.FileInfo) bool { return !i.IsDir() })
			if scope != tc.scope {
				t.Errorf("scope = %q, want %q", scope, tc.scope)
			}
			if walk := p.scopeWalk(tc.importer); !slices.Equal(walk, tc.walk) {
				t.Errorf("walk = %q, want %q", walk, tc.walk)
			}
			if modules := p.nearest(p.nms, parentDir(tc.importer), "node_modules", func(i fs.FileInfo) bool { return i.IsDir() }); modules != "\x00" {
				t.Errorf("node_modules = %q, want none", modules)
			}
		})
	}
}
