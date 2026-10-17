package typescript

import (
	"bytes"
	"cmp"
	"context"
	"encoding/gob"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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
	t.Cleanup(func() {
		pendingWrites.Wait()
		listingCacheDir, uncachedListing = "", runListingUncached
	})
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

func TestListingCache_SameSizeRestoredMtimeImportChangeIsNotServedStale(t *testing.T) {
	l := newListingRuns(t, patchFixture())
	l.list("app/tsconfig.json")
	a := filepath.Join(l.root, "app/src/a.ts")
	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte("import { b } from './c';\nexport const a = b;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(a, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	listed := l.list("app/tsconfig.json")
	if !slices.ContainsFunc(listed.Edges, func(e explainfiles.Edge) bool { return e.From == "app/src/a.ts" && e.To == "app/src/c.ts" }) {
		t.Fatalf("an in-place edit with its mtime restored kept the old import: %v", listed.Edges)
	}
	if diff := listingDifference(listed, l.fresh("app/tsconfig.json")); diff != "" {
		t.Errorf("the listing differs from tsgo's:\n%s", diff)
	}
}

func TestListingCache_AHardLinkToAPackageFileKeepsTheListing(t *testing.T) {
	l := newListingRuns(t, patchFixture())
	l.list("app/tsconfig.json")
	if err := os.Link(filepath.Join(l.root, "app/node_modules/react/index.d.ts"), filepath.Join(t.TempDir(), "index.d.ts")); err != nil {
		t.Fatal(err)
	}
	if l.list("app/tsconfig.json"); l.runs != 1 {
		t.Errorf("another link to a package file listed the program again (%d runs): %v", l.runs, lastPatchRefusal.Load())
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
	t.Cleanup(func() {
		pendingWrites.Wait()
		listingCacheDir, uncachedListing = "", runListingUncached
	})
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
		"app/node_modules/dual/package.json":      `{"name":"dual","version":"1.0.0","exports":{".":{"import":{"types":"./esm.d.ts"},"require":{"types":"./cjs.d.ts"}}}}`,
		"app/node_modules/dual/esm.d.ts":          "export declare const X: number;\n",
		"app/node_modules/dual/cjs.d.ts":          "export declare const X: number;\n",
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
	// An import added beside code the scan replay distrusts, which the patch reads as before.
	importBeside := func(code string) (start, edit map[string]string) {
		return map[string]string{"app/src/a.ts": "import { b } from './b';\n" + code + "\nexport const a = b;\n"},
			map[string]string{"app/src/a.ts": "import { b } from './b';\nimport { c } from './c';\n" + code + "\nexport const a = b + c;\n"}
	}
	type row struct {
		name        string
		start, edit map[string]string
		drop        []string
		check       func(*program) bool
	}
	besides := func(name, code string) row {
		start, edit := importBeside(code)
		return row{name: name, start: start, edit: edit}
	}
	for _, tc := range []row{
		besides("an import beside a string that reads like one", `export const doc = "import { x } from 'y'";`),
		besides("an import beside a namespace alias", "namespace N { export const y = 1; }\nimport y = N.y;\nexport const z = y;"),
		besides("an import beside a directive below the code", "export const z = 1;\n/// <reference lib=\"es2020\" />"),
		besides("an import beside a declare module holding an import", "declare module 'virtual' {\n  import { b } from './b';\n  export const v: typeof b;\n}"),
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
			tree := patchFixture()
			maps.Copy(tree, tc.start)
			l := newListingRuns(t, tree)
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
		name        string
		start, edit map[string]string
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
		{name: "a reference directive moved from below the code to the header", start: map[string]string{
			"app/src/a.ts": "import { b } from './b';\nexport const a = b;\n/// <reference types=\"react\" />\n",
		}, edit: map[string]string{
			"app/src/a.ts": "/// <reference types=\"react\" />\nimport { b } from './b';\nexport const a = b;\n",
		}},
		{name: "a jsxImportSource pragma moved out of a JSX expression to the header", start: map[string]string{
			"app/src/view/v.tsx": "import { useState } from 'react';\nexport const V = () => <div>{/** @jsxImportSource preact */ useState}</div>;\n",
		}, edit: map[string]string{
			"app/src/view/v.tsx": "/** @jsxImportSource preact */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}},
		{name: "an upper-case jsxImportSource pragma changed", start: map[string]string{
			"app/src/view/v.tsx": "/** @JSXImportSource react */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}, edit: map[string]string{
			"app/src/view/v.tsx": "/** @JSXImportSource preact */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}},
		{name: "a jsxImportSource pragma on a later line of its comment changed", start: map[string]string{
			"app/src/view/v.tsx": "/**\n * @jsxRuntime automatic\n * @jsxImportSource react\n */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}, edit: map[string]string{
			"app/src/view/v.tsx": "/**\n * @jsxRuntime automatic\n * @jsxImportSource preact\n */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}},
		{name: "two jsxImportSource pragmas swapped", start: map[string]string{
			"app/src/view/v.tsx": "/** @jsxImportSource preact */\n/** @jsxImportSource react */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}, edit: map[string]string{
			"app/src/view/v.tsx": "/** @jsxImportSource react */\n/** @jsxImportSource preact */\nimport { useState } from 'react';\nexport const V = () => <div>{useState}</div>;\n",
		}},
		{name: "an import.meta in a function removed beside an augmentation", start: map[string]string{
			"app/src/m.ts": "declare module './b' {\n  export const extra: number;\n}\nfunction f() { return import.meta.url; }\n",
		}, edit: map[string]string{
			"app/src/m.ts": "declare module './b' {\n  export const extra: number;\n}\nfunction f() { return 1; }\n",
		}},
		{name: "the last export removed before export as namespace beside an augmentation", start: map[string]string{
			"app/src/m.ts": "declare module './b' {\n  export const extra: number;\n}\nexport {};\nexport as namespace M;\n",
		}, edit: map[string]string{
			"app/src/m.ts": "declare module './b' {\n  export const extra: number;\n}\nexport as namespace M;\n",
		}},
		{name: "a resolution-mode import attribute changed", start: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution-mode': 'import' };\nexport type T = typeof X;\n",
		}, edit: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution-mode': 'require' };\nexport type T = typeof X;\n",
		}},
		{name: "an escaped resolution-mode import attribute changed", start: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution\\x2dmode': 'import' };\nexport type T = typeof X;\n",
		}, edit: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution\\x2dmode': 'require' };\nexport type T = typeof X;\n",
		}},
		{name: "a resolution-mode import attribute added", start: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual';\nexport type T = typeof X;\n",
		}, edit: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution-mode': 'require' };\nexport type T = typeof X;\n",
		}},
		{name: "a resolution-mode import attribute removed", start: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution-mode': 'require' };\nexport type T = typeof X;\n",
		}, edit: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual';\nexport type T = typeof X;\n",
		}},
		{name: "an import beside a resolution-mode attribute made type-only", start: map[string]string{
			"app/src/t.ts": "import { X } from 'dual' with { 'resolution-mode': 'require' };\nexport const T = X;\n",
		}, edit: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution-mode': 'require' };\nexport type T = typeof X;\n",
		}},
		{name: "an added file importing a package another file imports with a resolution-mode", start: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual' with { 'resolution-mode': 'require' };\nexport type T = typeof X;\n",
		}, edit: map[string]string{
			"app/src/u.ts": "import type { X } from 'dual';\nexport type U = typeof X;\n",
		}},
		{name: "an added file importing a package with a resolution-mode", start: map[string]string{
			"app/src/t.ts": "import type { X } from 'dual';\nexport type T = typeof X;\n",
		}, edit: map[string]string{
			"app/src/u.ts": "import type { X } from 'dual' with { 'resolution-mode': 'require' };\nexport type U = typeof X;\n",
		}},
		{name: "an import added beside a JavaScript file importing the package with an escaped resolution-mode", start: map[string]string{
			"app/tsconfig.json": strings.Replace(patchFixtureConfig, `"types":[]`, `"types":[],"allowJs":true,"checkJs":true`, 1),
			"app/src/t.js":      "/** @import { X } from 'dual' with { 'resolution\\u002dmode': 'require' } */\nexport const t = 1;\n",
		}, edit: map[string]string{
			"app/src/a.ts": "import { b } from './b';\nimport type { X } from 'dual';\nexport const a = b;\nexport type A = typeof X;\n",
		}},
		{name: "an added file importing a package an import type resolves with a resolution-mode", start: map[string]string{
			"app/src/t.ts": "export type T = import('dual', { with: { 'resolution-mode': 'require' } }).X;\n",
		}, edit: map[string]string{
			"app/src/u.ts": "import type { X } from 'dual';\nexport type U = typeof X;\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := patchFixture()
			maps.Copy(tree, tc.start)
			l := newListingRuns(t, tree)
			l.list("app/tsconfig.json")
			writeWorkspace(t, l.root, tc.edit)
			got := l.list("app/tsconfig.json")
			if diff := listingDifference(got, l.fresh("app/tsconfig.json")); diff != "" {
				t.Errorf("the listing differs from tsgo's:\n%s", diff)
			}
			if l.runs != 2 {
				t.Errorf("the edit was patched instead of listed again (%d runs)", l.runs)
			}
		})
	}
}

// tsgo resolves a dynamic import again for each "import" its span holds, a
// comment before it included, which read as an implicit import of the source.
const twiceResolvedSource = "export const load = () =>\n  // a static import here would cycle\n  import('./c');\n"

// A new or renamed source beside a twice-resolved import listed the program again;
// a source that resolves an import twice itself must still match tsgo.
func TestListingCache_AddOrRenameBesideATwiceResolvedImportPatches(t *testing.T) {
	addE := map[string]string{
		"app/src/e.ts": "export const e = 5;\n",
		"app/src/a.ts": "import { b } from './b';\nimport { e } from './e';\nexport const a = b + e;\n",
	}
	for _, tc := range []struct {
		name    string
		lazy    bool
		edit    map[string]string
		rename  [2]string
		roots   []string
		patched bool
	}{
		{name: "an added source an existing one imports", lazy: true, edit: addE, patched: true},
		{name: "a renamed source and its importer", lazy: true, rename: [2]string{"app/src/b.ts", "app/src/bee.ts"},
			edit: map[string]string{"app/src/a.ts": "import { b } from './bee';\nexport const a = b;\n"}, patched: true},
		{name: "an added root of a files list", lazy: true, edit: addE, roots: append(slices.Clone(projectionRoots), "app/src/e.ts"), patched: true},
		{name: "a renamed source under a files list", lazy: true, rename: [2]string{"app/src/b.ts", "app/src/bee.ts"},
			edit: map[string]string{"app/src/a.ts": "import { b } from './bee';\nexport const a = b;\n"}, roots: projectionRoots, patched: true},
		{name: "an added source that resolves an import twice", edit: map[string]string{
			"app/src/e.ts": twiceResolvedSource,
			"app/src/a.ts": addE["app/src/a.ts"],
		}},
		{name: "a twice-resolved import edited away", lazy: true, edit: map[string]string{"app/src/lazy.ts": "export const load = () => 1;\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := patchFixture()
			if tc.lazy {
				files["app/src/lazy.ts"] = twiceResolvedSource
			}
			base, roots := projectionRoots, tc.roots
			if tc.lazy && roots != nil {
				base, roots = append(slices.Clone(base), "app/src/lazy.ts"), append(slices.Clone(roots), "app/src/lazy.ts")
			}
			l := newListingRuns(t, files)
			l.listUnder(base, roots)
			if tc.rename[0] != "" {
				if err := os.Rename(filepath.Join(l.root, tc.rename[0]), filepath.Join(l.root, tc.rename[1])); err != nil {
					t.Fatal(err)
				}
			}
			writeWorkspace(t, l.root, tc.edit)
			got := l.listUnder(roots, roots)
			if patched := l.runs == 1; patched != tc.patched {
				t.Errorf("patched: %t, want %t (%d runs): %v", patched, tc.patched, l.runs, lastPatchRefusal.Load())
			}
			want := l.fresh("app/tsconfig.json")
			if roots != nil {
				want = l.freshProject("app/tsconfig.json", roots)
			}
			if diff := listingDifference(got, want); diff != "" {
				t.Errorf("the listing differs from tsgo's:\n%s", diff)
			}
		})
	}
}

// listUnder lists the fixture's program through the cache, as a files list
// projection of roots when wanted names one.
func (l *listingRuns) listUnder(roots, wanted []string) *program {
	if wanted != nil {
		return l.project("app/tsconfig.json", roots)
	}
	return l.list("app/tsconfig.json")
}

// A miscounted resolution made the patch add or keep one tsgo's listing does not
// hold; tsgo's own listing is the reference for every count the scanner settles.
func TestListingPatch_MiscountsNoResolutionTsgoRepeats(t *testing.T) {
	sources := []struct {
		file, body string
		counted    bool
	}{
		{"app/src/r1.ts", twiceResolvedSource, true},
		{"app/src/r2.ts", "// a static import here would cycle\nexport const load = async () => await import('./c');\n", true},
		{"app/src/r3.ts", "export const load = () => import(/* import */ './c');\n", true},
		{"app/src/r4.ts", "export const load = () => import('./c' /* require */);\n", true},
		{"app/src/r5.ts", "// import, import\nimport { b } from './b';\nexport const r = b;\n", true},
		{"app/src/r6.ts", "export const x = // import\n  import('./c');\n", true},
		{"app/src/r7.ts", "export type T = /* import */ typeof import('./c');\n", true},
		{"app/src/r8.ts", "export const load = () => import('./c').then((m) => m);\n", true},
		{"app/src/r9.ts", "export const load = () => import('./import-me');\n", false},
		{"app/src/r10.ts", "declare const api: { import(s: string): void };\nexport const x = api.import('./c');\n", false},
	}
	files := patchFixture()
	files["app/src/import-me.ts"] = "export const m = 1;\n"
	for _, s := range sources {
		files[s.file] = s.body
	}
	p := newListingRuns(t, files).list("app/tsconfig.json")
	for _, s := range sources {
		scanned, err := incremental.Scan(s.file, []byte(s.body))
		if err != nil {
			t.Fatal(err)
		}
		if scanned.Counted != s.counted {
			t.Errorf("%s: counted %t, want %t", s.file, scanned.Counted, s.counted)
		}
		if !scanned.Counted {
			continue
		}
		blocks := map[string]map[int]bool{}
		for _, c := range p.candidates {
			if c.from == s.file {
				if blocks[c.specifier] == nil {
					blocks[c.specifier] = map[int]bool{}
				}
				blocks[c.specifier][c.block] = true
			}
		}
		for _, text := range slices.Sorted(maps.Keys(blocks)) {
			if n := scanned.Resolutions[text]; n != len(blocks[text]) {
				t.Errorf("%s %q: counted %d resolutions, tsgo traced %d", s.file, text, n, len(blocks[text]))
			}
		}
		for text := range scanned.Resolutions {
			if blocks[text] == nil {
				t.Errorf("%s %q: counted, tsgo traced none", s.file, text)
			}
		}
	}
}

var listingCacheLine = regexp.MustCompile(`listing cache \d+ hit\(s\), (\d+) patched, (\d+) new, (\d+) stale`)

var listingCacheSummary = regexp.MustCompile(`(?m)^.*` + listingCacheLine.String() + `\n`)

// The same edits through a whole run: the stale listing patched, none listed
// again, and the BUILD files a cache-less run writes.
func TestListingCache_AddOrRenameBesideATwiceResolvedImportListsNothingAgain(t *testing.T) {
	requireTsgo(t)
	for _, edit := range []struct {
		name string
		do   func(t *testing.T, root string)
	}{
		{"an added source an existing one imports", func(t *testing.T, root string) {
			writeWorkspace(t, root, map[string]string{
				"app/f.ts": "export const f = 6;\n",
				"app/d.ts": "import { f } from './f';\nexport const d = f;\n",
			})
		}},
		{"a renamed source and its importers", func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "app/b.ts"), filepath.Join(root, "app/bee.ts")); err != nil {
				t.Fatal(err)
			}
			writeWorkspace(t, root, map[string]string{
				"app/a.ts": "import { b } from './bee';\nimport { x } from '../lib/x';\nexport const a = b + x;\n",
				"app/c.ts": "import { b } from './bee';\nexport const c = b;\n",
			})
		}},
	} {
		t.Run(edit.name, func(t *testing.T) {
			t.Parallel()
			tree := fastPathTree()
			tree["app/lazy.ts"] = twiceResolvedSource
			root := writeTree(t, tree)
			cache := t.TempDir()
			primeFastPath(t, root, cache, "-ts_verbose")
			edit.do(t, root)
			same := requireAsCacheless(t, root, edit.name)
			out := recordGazelle(t, root, cache, "-ts_fastpath=off")
			if m := listingCacheLine.FindStringSubmatch(out); m == nil || m[1] == "0" || m[2] != "0" || m[3] != "0" {
				t.Errorf("%s listed a program again instead of patching it:\n%s", edit.name, out)
			}
			same()
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

// A cold run listed a files list over its config as tsgo was handed it, and tsgo
// explains each root of a files list by a scan of the whole list: minutes for
// thousands of roots. A list adding roots to its config's listing from the run
// is listed through the config's include instead, where tsgo roots the same files.
func TestListingCache_FilesListOverItsConfigListsThroughTheInclude(t *testing.T) {
	inInclude := []string{"app/src/a.ts", "app/src/b.ts", "app/src/c.ts", "app/src/lib/d.ts", "app/src/lib/index.ts", "app/src/view/v.tsx", "app/src/view/w.tsx"}
	outside := map[string]string{"app/scripts/s.ts": "import { b } from '../src/b';\nexport const s = b;\n"}
	for _, tc := range []struct {
		name     string
		edit     map[string]string
		roots    []string
		narrowed bool
		unlisted bool
	}{
		{name: "a root the include does not match", edit: outside, roots: append(slices.Clone(inInclude), "app/scripts/s.ts"), narrowed: true},
		{name: "a script tsgo does not root", roots: append(slices.Clone(inInclude), "app/tool.mjs"), narrowed: true},
		{name: "a root the config excludes", edit: map[string]string{
			"app/tsconfig.json": strings.Replace(patchFixtureConfig, `"include"`, `"exclude":["src/view/w.tsx"],"include"`, 1),
		}, roots: inInclude, narrowed: true},
		{name: "no listing of the config's own in the run", edit: outside, roots: append(slices.Clone(inInclude), "app/scripts/s.ts"), unlisted: true},
		{name: "a dropped root", edit: outside, roots: []string{"app/scripts/s.ts", "app/src/a.ts", "app/src/c.ts"}},
		{name: "a twin of a root with an extension the include ranks below the root's",
			edit:  map[string]string{"app/src/b.d.ts": "export declare const b: number;\n"},
			roots: append(slices.Clone(inInclude), "app/src/b.d.ts"), narrowed: true},
		{name: "a twin of a root with an extension the include ranks above the root's", edit: map[string]string{
			"app/src/b.tsx":     "export const b = 1;\n",
			"app/tsconfig.json": strings.Replace(patchFixtureConfig, `"include"`, `"exclude":["src/b.ts"],"include"`, 1),
		}, roots: append(slices.Clone(inInclude), "app/src/b.tsx")},
		{name: "a config with files of its own", edit: map[string]string{
			"app/scripts/s.ts":  outside["app/scripts/s.ts"],
			"app/scripts/t.ts":  "export const t = 1;\n",
			"app/tsconfig.json": strings.Replace(patchFixtureConfig, `"include"`, `"files":["scripts/s.ts"],"include"`, 1),
		}, roots: append(slices.Clone(inInclude), "app/scripts/s.ts", "app/scripts/t.ts")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListingRuns(t, patchFixture())
			writeWorkspace(t, l.root, tc.edit)
			var narrowed bool
			uncachedListing = func(ctx context.Context, repoRoot, tsgo, subject string, args []string) (*program, error) {
				l.runs++
				for _, arg := range args {
					if data, err := os.ReadFile(arg); isProjection(arg) && err == nil && !strings.Contains(string(data), `"include"`) {
						narrowed = true
					}
				}
				return runListingUncached(ctx, repoRoot, tsgo, subject, args)
			}
			listingNames, listingStats = newListingNames(), &incremental.StatCache{}
			sourceSpecs, loadedListings = sync.Map{}, sync.Map{}
			if !tc.unlisted {
				if _, err := listCompilerProgram(l.root, "app/tsconfig.json", l.tsgo, nil); err != nil {
					t.Fatal(err)
				}
			}
			got, err := listCompilerProgram(l.root, "app/tsconfig.json", l.tsgo, nil, tc.roots...)
			if err != nil {
				t.Fatal(err)
			}
			pendingWrites.Wait()
			if narrowed != tc.narrowed {
				t.Errorf("listed through the include: %t, want %t", narrowed, tc.narrowed)
			}
			if diff := listingDifference(got, l.freshProject("app/tsconfig.json", tc.roots)); diff != "" {
				t.Errorf("the files list's listing differs from tsgo's:\n%s", diff)
			}
		})
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

const aliasFixtureConfig = `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler","jsx":"react-jsx","types":[],"paths":{` +
	`"#modules/*":["./modules/*"],"#shared/*":["./shared/*"],"#unused/*":["./shared/*"],"#two/*":["./a/*","./b/*"],` +
	`"#m/*":["./modules/*"],"#m/legal/*":["./modules/legal/*"],"#auth":["./shared/util.ts"]}},` +
	`"include":["src/**/*","modules/**/*","shared/**/*"]}`

// aliasFixture is patchFixture with paths aliases that existing sources resolve,
// so the listing's trace shows how each used pattern maps.
func aliasFixture() map[string]string {
	files := patchFixture()
	files["app/package.json"] = `{"name":"app","imports":{"#modules/*":"./modules/*","#lib/*":"./shared/*"}}`
	files["app/tsconfig.json"] = aliasFixtureConfig
	files["app/modules/page/P.tsx"] = "export const P = 1;\n"
	files["app/modules/page/index.ts"] = "export * from './P';\n"
	files["app/shared/util.ts"] = "export const util = 1;\n"
	files["app/shared/fmt.ts"] = "import { util } from '#shared/util';\nexport const fmt = util;\n"
	files["app/src/routes.ts"] = "import { P } from '#modules/page/P';\nimport { m } from '#m/page/P';\nimport { x } from '#two/x';\nexport const routes = [P, m, x];\n"
	files["app/a/x.ts"] = "export const x = 1;\n"
	return files
}

var aliasProjectionRoots = []string{"app/shared/fmt.ts", "app/src/a.ts", "app/src/routes.ts", "app/src/view/v.tsx"}

const aliasPage = "app/modules/legal/terms/Page.tsx"

// routesImporting is routes.ts with one more import, of spec.
func routesImporting(spec string) string {
	return "import { P } from '#modules/page/P';\nimport { m } from '#m/page/P';\nimport { x } from '#two/x';\nimport { Page } from '" + spec + "';\nexport const routes = [P, m, x, Page];\n"
}

// A source imported only through a paths alias sent the whole program back to
// tsgo: the patch resolved a bare specifier only by copying another importer's.
func TestListingCache_AliasImportPatchesAsTsgoResolvesIt(t *testing.T) {
	page := map[string]string{aliasPage: "import { fmt } from '#shared/fmt';\nexport const Page = () => <div>{fmt}</div>;\n"}
	for _, tc := range []struct {
		name       string
		base, edit map[string]string
		roots      []string
		cfg        string
	}{
		{name: "an added page under a config that inherits its paths from another directory", base: map[string]string{
			"app/nested/deeper/tsconfig.json": `{"extends":"../../tsconfig.json","include":["../../src/**/*","../../modules/**/*","../../shared/**/*"]}`,
		}, edit: map[string]string{
			"app/src/routes.ts": routesImporting("#modules/legal/terms/Page"),
		}, cfg: "app/nested/deeper/tsconfig.json"},
		{name: "an alias that found nothing finds an added file", base: map[string]string{
			"app/src/later.ts": "import { Page } from '#modules/legal/terms/Page';\nexport const later = Page;\n",
		}},
		{name: "an added page in a new directory imported only through an alias", edit: map[string]string{
			"app/src/routes.ts": routesImporting("#modules/legal/terms/Page"),
		}},
		{name: "an alias import of a directory's index", edit: map[string]string{
			"app/src/a.ts": "import { b } from './b';\nimport { P } from '#modules/page';\nexport const a = b + P;\n",
		}},
		{name: "a changed source gains an alias import no other source writes", edit: map[string]string{
			"app/src/c.ts": "import { fmt } from '#shared/fmt';\nexport const c = fmt;\n",
		}},
		{name: "an added page under a files list", edit: map[string]string{
			"app/src/routes.ts": routesImporting("#modules/legal/terms/Page"),
		}, roots: append(slices.Clone(aliasProjectionRoots), aliasPage)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := aliasFixture()
			maps.Copy(files, tc.base)
			l := newListingRuns(t, files)
			cfg := cmp.Or(tc.cfg, "app/tsconfig.json")
			var base []string
			if tc.roots != nil {
				base = aliasProjectionRoots
			}
			l.project(cfg, base)
			writeWorkspace(t, l.root, page)
			writeWorkspace(t, l.root, tc.edit)
			got := l.project(cfg, tc.roots)
			if l.runs != 1 {
				t.Fatalf("the edit listed the program again (%d runs): %v", l.runs, lastPatchRefusal.Load())
			}
			want := l.freshProject(cfg, tc.roots)
			if diff := listingDifference(got, want); diff != "" {
				t.Errorf("the patched listing differs from tsgo's:\n%s", diff)
			}
		})
	}
}

// An alias whose resolution the trace does not pin down goes back to tsgo.
func TestListingCache_AliasImportTheTraceDoesNotProveListsAgain(t *testing.T) {
	for _, tc := range []struct {
		name, spec, why string
	}{
		{"an alias that resolves nowhere falls through to package imports", "#modules/legal/gone/Page", "finds nothing"},
		{"a pattern with a fallback substitution", "#two/y", "fallback substitution"},
		{"two patterns match the specifier", "#m/legal/terms/Page", "more than one paths pattern"},
		{"a pattern no traced resolution used", "#unused/util", "no traced resolution"},
		{"an exact key whose substitution names a file", "#auth", "exact paths key"},
		{"a specifier only the package imports map names", "#lib/util", "no importer in the same scope"},
		{"an alias with a TypeScript extension", "#modules/legal/terms/Page.tsx", "TypeScript extension"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListingRuns(t, aliasFixture())
			l.list("app/tsconfig.json")
			writeWorkspace(t, l.root, map[string]string{
				aliasPage:           "export const Page = 1;\n",
				"app/b/y.ts":        "export const y = 1;\n",
				"app/src/routes.ts": routesImporting(tc.spec),
			})
			got := l.list("app/tsconfig.json")
			if l.runs != 2 {
				t.Fatalf("the edit was patched instead of listed again (%d runs)", l.runs)
			}
			if why, _ := lastPatchRefusal.Load().(string); !strings.Contains(why, tc.why) {
				t.Errorf("listed again because %q, want %q", why, tc.why)
			}
			if diff := listingDifference(got, l.fresh("app/tsconfig.json")); diff != "" {
				t.Errorf("the relisted program differs from tsgo's:\n%s", diff)
			}
		})
	}
}

// Through a whole run: a new source imported only through an alias patches
// every listing, none is listed again, and tsgo's own listing agrees.
func TestListingCache_AliasImportedAddListsNothingAgain(t *testing.T) {
	requireTsgo(t)
	tree := fastPathTree()
	tree["app/c.ts"] = "import { b } from '@app/b';\nexport const c = b;\n"
	root := writeTree(t, tree)
	cache := t.TempDir()
	primeFastPath(t, root, cache, "-ts_verbose")
	writeWorkspace(t, root, map[string]string{
		"app/f.ts": "export const f = 6;\n",
		"app/d.ts": "import { f } from '@app/f';\nexport const d = f;\n",
	})
	same := requireAsCacheless(t, root, "an alias-imported add")
	out := recordGazelle(t, root, cache, "-ts_fastpath=off", "-ts_listing_patch_verify")
	if m := listingCacheLine.FindStringSubmatch(out); m == nil || m[1] == "0" || m[2] != "0" || m[3] != "0" {
		t.Errorf("an alias-imported add listed a program again instead of patching it:\n%s", out)
	}
	same()
}

// The verifier stands for tsgo wherever a listing is patched, so a fact that
// later eligibility consumes must not compare equal when it differs.
func TestListingParity_RejectsChangesToConsumedFacts(t *testing.T) {
	typescript := "TypeScript, Declaration."
	listed := func() *program {
		return &program{
			Listing: explainfiles.Listing{
				Files: []string{"app/a.ts", "app/b.ts", "app/g.d.ts"},
				Roots: []string{"app/a.ts", "app/b.ts"},
				Types: []explainfiles.TypeEntry{{Entry: "g", File: "app/g.d.ts"}},
				Unresolved: []explainfiles.Unresolved{{Kind: explainfiles.Reference, From: "app/a.ts", Specifier: "./gone.d.ts",
					Candidates: []explainfiles.FailedLookup{{File: "app/gone.d.ts"}}}},
			},
			candidates: []resolutionCandidate{{from: "app/b.ts", specifier: "#lib/x", path: "app/lib/x.ts", file: true}},
			resolution: resolutionFacts{
				Kinds:           []string{"Module resolution kind is not specified, using 'Bundler'."},
				RelativeTargets: []string{typescript}, Substitutions: []string{"#lib/*\x00./lib/*\x00app"}, SubstitutionTargets: []string{typescript},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		change func(got, want *program)
	}{
		{"an extra edge where tsgo lists none", func(got, _ *program) {
			got.Edges = []explainfiles.Edge{{Kind: explainfiles.Import, From: "app/a.ts", To: "app/b.ts", Specifier: "./b"}}
		}},
		{"a type entry moved from explicit to implicit", func(got, _ *program) { got.Types, got.Implicit = nil, got.Types }},
		{"a missing unresolved import", func(got, _ *program) { got.Unresolved = nil }},
		{"a changed probe of an unresolved import", func(got, _ *program) {
			got.Unresolved[0].Candidates = []explainfiles.FailedLookup{{File: "app/gone.ts"}}
		}},
		{"a changed paths substitution", func(got, _ *program) { got.resolution.Substitutions = []string{"#lib/*\x00./src/*\x00app"} }},
		{"a relative target tsgo traces is missing", func(got, _ *program) { got.resolution.RelativeTargets = []string{"JavaScript."} }},
		{"a second resolution kind dropped", func(_, want *program) {
			want.resolution.Kinds = append(want.resolution.Kinds, "Explicitly specified module resolution kind: 'Node10'.")
		}},
		{"a dropped rootDirs fact", func(_, want *program) { want.resolution.RootDirs = true }},
		{"a listing tsgo refused", func(_, want *program) { want.refused = "tsgo exit status 2: error TS5083: Cannot read file" }},
		{"a changed refusal", func(got, want *program) {
			got.refused, want.refused = "a config in the chain is unreadable", "tsgo exit status 2: error TS5083: Cannot read file"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, want := listed(), listed()
			tc.change(got, want)
			if listingDifference(got, want) == "" {
				t.Error("the verifier found no difference")
			}
		})
	}
	t.Run("order, block numbering and facts kept from a dropped resolution", func(t *testing.T) {
		got, want := listed(), listed()
		slices.Reverse(got.Files)
		got.candidates[0].block = 7
		got.resolution.Substitutions = append(got.resolution.Substitutions, "#old/*\x00./old/*\x00app")
		if diff := listingDifference(got, want); diff != "" {
			t.Errorf("the verifier rejects an equivalent listing:\n%s", diff)
		}
	})
}

// A patch keeps the facts of an import it drops, and a later edit may patch by them.
func TestListingCache_ASecondEditPatchesByARetainedFact(t *testing.T) {
	l := newListingRuns(t, aliasFixture())
	l.list("app/tsconfig.json")
	writeWorkspace(t, l.root, map[string]string{"app/shared/fmt.ts": "export const fmt = 1;\n"})
	dropped, fresh := l.list("app/tsconfig.json"), l.fresh("app/tsconfig.json")
	if l.runs != 1 || len(dropped.resolution.Substitutions) <= len(fresh.resolution.Substitutions) {
		t.Fatalf("dropping the only #shared import did not patch and retain its substitution (%d runs): %v", l.runs, lastPatchRefusal.Load())
	}
	if diff := listingDifference(dropped, fresh); diff != "" {
		t.Errorf("the verifier rejects a patch that retains a fact:\n%s", diff)
	}
	writeWorkspace(t, l.root, map[string]string{"app/shared/fmt.ts": "import { util } from '#shared/util';\nexport const fmt = util;\n"})
	if got := l.list("app/tsconfig.json"); l.runs != 1 {
		t.Errorf("the retained substitution did not patch the import that uses it (%d runs): %v", l.runs, lastPatchRefusal.Load())
	} else if diff := listingDifference(got, l.fresh("app/tsconfig.json")); diff != "" {
		t.Errorf("the patch that used a retained fact differs from tsgo's:\n%s", diff)
	}
}

// An older binary sharing the cache decodes an entry's first value as a whole
// cachedListing; a head it took for one would hold no reads, and its fast path would replay.
func TestListingCache_AnOlderBinaryCannotTakeAnEntryHeadForAnEntry(t *testing.T) {
	var file bytes.Buffer
	entry := &cachedListing{Root: "/repo", Sources: map[string]cachedSource{"a.ts": {Specs: []string{"./b"}}}, FilesList: []string{"a.ts"},
		Reads: []string{"/repo/a.ts"}, ReadLines: []string{"f"}, PatchVersion: patchStateVersion}
	if err := encodeEntry(&file, entry); err != nil {
		t.Fatal(err)
	}
	var old cachedListing
	if err := gob.NewDecoder(&file).Decode(&old); err == nil {
		t.Errorf("an entry's head decodes as a cachedListing with %d read(s) and root %q", len(old.Reads), old.Root)
	}
}

// A cache hit dropped tsgo's refusal of a program, so a rule keeping sources
// over it was generated where a cache-less run fails.
func TestListingCache_ARefusedProgramStaysRefusedOnACacheHit(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, map[string]string{
		"MODULE.bazel":      "module(name = \"refused_program\")\n",
		"BUILD.bazel":       "",
		"app/tsconfig.json": `{"compilerOptions":{"noLib":true,"types":[]},"files":["missing.ts"]}`,
		"app/a.ts":          "export const a = 1;\n",
	})
	// A failing run may exit before its entry is written, so a passing run writes it.
	cache := t.TempDir()
	if out, err := tsGazelle(t, root, "-ts_listing_cache="+cache); err != nil {
		t.Fatalf("the run writing the entry: %v\n%s", err, out)
	}
	if names, _ := os.ReadDir(cache); !slices.ContainsFunc(names, func(e os.DirEntry) bool { return isListingKey(e.Name()) }) {
		t.Fatalf("the run wrote no listing entry: %v", names)
	}
	writeWorkspace(t, root, map[string]string{
		"app/BUILD.bazel": loadDefs + "\"ts_compile\")\nts_compile(\n    name = \"app\",\n    srcs = [\"a.ts\"],  # keep\n)\n",
	})
	reference := copyTree(t, root)
	want, wantErr := tsGazelle(t, reference)
	want = strings.ReplaceAll(want, reference, root)
	got, err := tsGazelle(t, root, "-ts_listing_cache="+cache)
	got = listingCacheSummary.ReplaceAllString(got, "")
	if wantErr == nil || err == nil || got != want {
		t.Fatalf("a cache hit: gazelle %v, a cache-less run %v:\n%s\nwant:\n%s", err, wantErr, got, want)
	}
	if diff := snapshotDiff(buildFileBytes(t, reference), buildFileBytes(t, root)); diff != "" {
		t.Fatalf("a cache hit: BUILD files differ from a cache-less run: %s", diff)
	}
}

// A patch that drops a program's last file kept it listed where tsgo refuses it.
func TestListingCache_APatchThatEmptiesARefusableProgramListsItAgain(t *testing.T) {
	l := newListingRuns(t, map[string]string{
		"app/tsconfig.json": `{"compilerOptions":{"noLib":true,"types":[],"strict":"yes"},"include":["src"]}`,
		"app/src/a.ts":      "export const a = 1;\n",
	})
	if listed := l.list("app/tsconfig.json"); listed.refused != "" || len(listed.Diagnostics) == 0 {
		t.Fatalf("want a listed program with a diagnostic, got refusal %q, diagnostics %v", listed.refused, listed.Diagnostics)
	}
	if err := os.Remove(filepath.Join(l.root, "app/src/a.ts")); err != nil {
		t.Fatal(err)
	}
	got, want := l.list("app/tsconfig.json"), l.fresh("app/tsconfig.json")
	if want.refused == "" || got.refused == "" {
		t.Errorf("a cached run refused %q with files %v, tsgo refused %q", got.refused, got.Files, want.refused)
	}
}

// A files entry missing when tsgo listed was no read of the listing, so a cache
// hit kept the program without it once it was written.
func TestListingCache_AFilesEntryWrittenAfterTheListingIsListed(t *testing.T) {
	requireTsgo(t)
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"relative", map[string]string{"app/tsconfig.json": `{"compilerOptions":{"noLib":true,"types":[]},"files":["a.ts","late.ts"]}`}},
		{"${configDir} in an extended config", map[string]string{
			"base/tsconfig.json": `{"compilerOptions":{"noLib":true,"types":[]},"files":["${configDir}/a.ts","${configDir}/late.ts"]}`,
			"app/tsconfig.json":  `{"extends":"../base/tsconfig.json"}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"MODULE.bazel": "module(name = \"files_entry\")\n",
				"BUILD.bazel":  "",
				"app/a.ts":     "export const a = 1;\n",
			})
			writeWorkspace(t, root, tc.files)
			cache := t.TempDir()
			recordGazelle(t, root, cache)
			writeWorkspace(t, root, map[string]string{"app/late.ts": "export const late = 2;\n"})
			same := requireAsCacheless(t, root, "a files entry written after the listing")
			recordGazelle(t, root, cache)
			same()
		})
	}
}
