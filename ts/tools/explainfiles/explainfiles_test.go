package explainfiles

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	sample    = "testdata/workers_download_test.listing.txt"
	store     = "../../../.local/share/pnpm/store/v11/links/"
	storeNode = store + "@types/node/22.13.13/" +
		"9435199b93d373e137e4c716aec7ae4974ab7bb876d51e74a0919a6d6d780519" +
		"/node_modules/@types/node/index.d.ts"
	storeUndici = store + "@/undici-types/6.20.0/" +
		"91f3a19b95acf703b5bb73429c4b4f8a36342005ce1ad1bb380fcbef595f5a3b" +
		"/node_modules/undici-types/formdata.d.ts"
	storePoolTypes = store + "@cloudflare/vitest-pool-workers/0.18.4/" +
		"8648841c094c3e6cd8f80b31b4203386c6154c0a84ccf11aa2c8b9b382106bb9" +
		"/node_modules/@cloudflare/vitest-pool-workers/types/cloudflare-test.d.ts"
)

func parseSample(t *testing.T) *Listing {
	t.Helper()
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse(string(data))
	if err != nil {
		t.Fatalf("Parse(%s): %v", sample, err)
	}
	return l
}

func TestParse_SampleRoots(t *testing.T) {
	l := parseSample(t)
	want := []string{
		"workers/download/test/env.d.ts",
		"workers/download/test/index.spec.ts",
		"workers/download/worker-configuration.d.ts",
	}
	if !slices.Equal(l.Roots, want) {
		t.Errorf("roots = %q, want %q", l.Roots, want)
	}
	if len(l.Files) != 208 {
		t.Errorf("files listed = %d, want 208 (the file lines of the sample)",
			len(l.Files))
	}
	if len(l.Edges) != 352 {
		t.Errorf("edges = %d, want 352 (283 imports, 61 references, "+
			"8 type-library references)", len(l.Edges))
	}
	if len(l.Diagnostics) != 0 {
		t.Errorf("diagnostics = %q, want none", l.Diagnostics)
	}
}

func TestParse_SampleEdgeIntoAnotherProgram(t *testing.T) {
	l := parseSample(t)
	want := Edge{
		Kind:      Import,
		From:      "workers/download/test/index.spec.ts",
		To:        "workers/download/src/index.ts",
		Specifier: "../src/index",
	}
	if !slices.Contains(l.Edges, want) {
		t.Errorf("no edge %+v; the edges from the test file are %+v", want,
			edgesFrom(l, want.From))
	}
	typeRef := Edge{
		Kind:      TypeReference,
		From:      storeUndici,
		To:        storeNode,
		Specifier: "node",
	}
	if !slices.Contains(l.Edges, typeRef) {
		t.Errorf("no /// <reference types> edge %+v", typeRef)
	}
}

func edgesFrom(l *Listing, from string) []Edge {
	var out []Edge
	for _, e := range l.Edges {
		if e.From == from {
			out = append(out, e)
		}
	}
	return out
}

func TestParse_SampleTypeEntriesVerbatim(t *testing.T) {
	l := parseSample(t)
	want := []TypeEntry{
		{Entry: "../worker-configuration.d.ts",
			File: "workers/download/worker-configuration.d.ts"},
		{Entry: "@cloudflare/vitest-pool-workers/types", File: storePoolTypes},
		{Entry: "node", File: storeNode},
	}
	if !slices.Equal(l.Types, want) {
		t.Errorf("types = %+v, want %+v", l.Types, want)
	}
	if len(l.Implicit) != 0 {
		t.Errorf("implicit type libraries = %+v, want none: the tsconfig "+
			"has a types key", l.Implicit)
	}
}

func TestParse_ReasonForms(t *testing.T) {
	cases := []struct{ line, want string }{
		{`Imported via "./b" from file 'pkg/src/a.ts'`,
			"import ./b from pkg/src/a.ts"},
		{`Imported via "react" from file 'pkg/src/a.ts'` +
			` with packageId 'react/index.d.ts@19.0.0'`,
			"import react from pkg/src/a.ts"},
		{`Imported via "react/jsx-runtime" from file 'pkg/src/a.ts'` +
			` to import 'jsx' and 'jsxs' factory functions`,
			"import react/jsx-runtime from pkg/src/a.ts"},
		{`Imported via "react/jsx-runtime" from file 'pkg/src/a.ts'` +
			` with packageId 'react/jsx-runtime.d.ts@19.0.0'` +
			` to import 'jsx' and 'jsxs' factory functions`,
			"import react/jsx-runtime from pkg/src/a.ts"},
		{`Imported via "tslib" from file 'pkg/src/a.ts'` +
			` to import 'importHelpers' as specified in compilerOptions`,
			"import tslib from pkg/src/a.ts"},
		{`Imported via "tslib" from file 'pkg/src/a.ts'` +
			` with packageId 'tslib/tslib.d.ts@2.8.1'` +
			` to import 'importHelpers' as specified in compilerOptions`,
			"import tslib from pkg/src/a.ts"},
		{`Augmented via "vite" from file 'pkg/src/a.ts'` +
			` with packageId 'vite/dist/node/index.d.ts@8.2.2'`,
			"augment vite from pkg/src/a.ts"},
		{`Augmented via "./lib" from file 'pkg/src/a.ts'`,
			"augment ./lib from pkg/src/a.ts"},
		{`Referenced via './globals.d.ts' from file 'pkg/src/a.ts'`,
			"reference ./globals.d.ts from pkg/src/a.ts"},
		{`Type library referenced via 'node' from file 'pkg/src/a.ts'`,
			"typeref node from pkg/src/a.ts"},
		{`Type library referenced via 'node' from file 'pkg/src/a.ts'` +
			` with packageId '@types/node/index.d.ts@22.13.13'`,
			"typeref node from pkg/src/a.ts"},
		{`Entry point of type library 'node' specified in compilerOptions`,
			"types node"},
		{`Entry point of type library 'node' specified in compilerOptions` +
			` with packageId '@types/node/index.d.ts@22.13.13'`,
			"types node"},
		{`Entry point for implicit type library 'node'`, "implicit node"},
		{`Entry point for implicit type library 'node'` +
			` with packageId '@types/node/index.d.ts@22.13.13'`,
			"implicit node"},
		{`Matched by include pattern 'src/**/*.ts' in 'pkg/tsconfig.json'`,
			"root"},
		{`Matched by default include pattern '**/*'`, "root"},
		{`Part of 'files' list in tsconfig.json`, "root"},
		{`Root file specified for compilation`, "root"},
		{`Library referenced via 'es2015' from file 'pkg/src/a.ts'`,
			"nothing"},
		{`Library 'lib.es2022.d.ts' specified in compilerOptions`, "nothing"},
		{`Default library for target 'es2022'`, "nothing"},
		{`Default library`, "nothing"},
		{`File is ECMAScript module because 'pkg/package.json'` +
			` has field "type" with value "module"`, "nothing"},
		{`File is CommonJS module because 'pkg/package.json'` +
			` has field "type" whose value is not "module"`, "nothing"},
		{`File is CommonJS module because 'pkg/package.json'` +
			` does not have field "type"`, "nothing"},
		{`File is CommonJS module because 'package.json' was not found`,
			"nothing"},
		{`File redirects to file 'pkg/node_modules/.pnpm/react@19.0.0` +
			`/node_modules/react/index.d.ts'`, "nothing"},
	}
	if len(cases) != len(reasonForms) {
		t.Fatalf("%d forms in this table, %d in the parser", len(cases),
			len(reasonForms))
	}
	for _, c := range cases {
		l, err := Parse("pkg/src/target.ts\n   " + c.line + "\n")
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		if got := readingOf(l); got != c.want {
			t.Errorf("%s:\n  read as %q, want %q", c.line, got, c.want)
		}
	}
}

func TestParse_SpecifierQuoteSpelling(t *testing.T) {
	for _, line := range []string{
		`Imported via './b' from file 'pkg/src/a.ts'`,
		`Imported via 'react' from file 'pkg/src/a.ts'` +
			` with packageId 'react/index.d.ts@19.0.0'`,
		`Imported via "it's" from file 'pkg/src/a.ts'`,
	} {
		l, err := Parse("pkg/src/target.ts\n   " + line + "\n")
		if err != nil {
			t.Errorf("%s: %v", line, err)
			continue
		}
		if len(l.Edges) != 1 || l.Edges[0].Kind != Import ||
			l.Edges[0].From != "pkg/src/a.ts" {
			t.Errorf("%s: read as %+v", line, l)
		}
	}
}

// tsgo prints a path as it is, quotes included; the grammar must not end the
// path at one, nor read a form's own suffix into it.
func TestParse_QuotesInsidePaths(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{`Imported via "./b" from file 'pkg/o'brien/a.ts'`,
			"import ./b from pkg/o'brien/a.ts"},
		{`Imported via './b' from file 'pkg/o'brien/a.ts'` +
			` with packageId 'b/index.d.ts@1.0.0'`,
			"import ./b from pkg/o'brien/a.ts"},
		{`Imported via "react/jsx-runtime" from file 'pkg/o'brien/a.tsx'` +
			` with packageId 'react/jsx-runtime.d.ts@19.0.0'` +
			` to import 'jsx' and 'jsxs' factory functions`,
			"import react/jsx-runtime from pkg/o'brien/a.tsx"},
		{`Imported via "it's" from file 'pkg/o'brien/a.ts'`,
			"import it's from pkg/o'brien/a.ts"},
		{`Referenced via './x.d.ts' from file 'pkg/o'brien/a.ts'`,
			"reference ./x.d.ts from pkg/o'brien/a.ts"},
		{`Type library referenced via 'node' from file 'pkg/o'brien/a.ts'` +
			` with packageId '@types/node/index.d.ts@22.13.13'`,
			"typeref node from pkg/o'brien/a.ts"},
		{`Matched by include pattern 'src/**/*.ts' in 'pkg/o'brien/tsconfig.json'`,
			"root"},
		{`File redirects to file 'pkg/o'brien/node_modules/react/index.d.ts'`,
			"nothing"},
	} {
		l, err := Parse("pkg/src/target.ts\n   " + c.line + "\n")
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		if got := readingOf(l); got != c.want {
			t.Errorf("%s:\n  read as %q, want %q", c.line, got, c.want)
		}
	}
}

func readingOf(l *Listing) string {
	switch {
	case len(l.Roots) == 1:
		return "root"
	case len(l.Edges) == 1:
		e := l.Edges[0]
		kind := map[EdgeKind]string{Import: "import", Reference: "reference",
			TypeReference: "typeref", Augmentation: "augment"}[e.Kind]
		return fmt.Sprintf("%s %s from %s", kind, e.Specifier, e.From)
	case len(l.Types) == 1:
		return "types " + l.Types[0].Entry
	case len(l.Implicit) == 1:
		return "implicit " + l.Implicit[0].Entry
	case len(l.Roots)+len(l.Edges)+len(l.Types)+len(l.Implicit) == 0:
		return "nothing"
	}
	return fmt.Sprintf("%+v", l)
}

func TestParse_UnknownReasonLineIsAnError(t *testing.T) {
	const line = `Output from referenced project 'pkg/lib/tsconfig.json'` +
		` included because '--module' is specified as 'none'`
	_, err := Parse("pkg/src/a.ts\n   " + line + "\n")
	if err == nil {
		t.Fatal("a reason line the grammar does not know parsed without error")
	}
	if !strings.Contains(err.Error(), line) {
		t.Errorf("the error does not quote the line:\n%v", err)
	}
	_, err = Parse("   Matched by default include pattern '**/*'\n")
	if err == nil {
		t.Error("a reason line before any file line parsed without error")
	}
}

// What tsgo prints, with exit 2, for a tsconfig.json whose include matches
// nothing.
const noInputsOutput = `error TS18003: No inputs were found in config file ` +
	`'/w/pkg/tsconfig.json'. Specified 'include' paths were ` +
	`'["src/**/*.ts","bin/*.ts"]' and 'exclude' paths were '["node_modules"]'.`

func TestParse_NoInputsIsZeroRoots(t *testing.T) {
	l, err := Parse(noInputsOutput + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Roots) != 0 || len(l.Files) != 0 {
		t.Errorf("roots = %q, files = %q, want none", l.Roots, l.Files)
	}
	if len(l.Diagnostics) != 1 || !strings.Contains(l.Diagnostics[0], "TS18003") {
		t.Errorf("diagnostics = %q, want the TS18003 line", l.Diagnostics)
	}

	// A diagnostic's continuation lines are indented two spaces per level of
	// its message chain, a reason three; the listing follows the diagnostics.
	withListing := "eslint-plugin/tsconfig.json(31,5): error TS5102: Option " +
		"'baseUrl' has been removed. Please remove it from your configuration.\n" +
		"  Use '\"paths\": {\"*\": [\"./*\"]}' instead.\n" +
		"    A second level of the chain, indented two more.\n" +
		"eslint-plugin/src/index.ts\n" +
		"   Matched by include pattern 'src/**/*.ts' in " +
		"'eslint-plugin/tsconfig.json'\n"
	l, err = Parse(withListing)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Roots, []string{"eslint-plugin/src/index.ts"}) {
		t.Errorf("roots = %q, want the one include match", l.Roots)
	}
	if len(l.Diagnostics) != 1 || strings.Count(l.Diagnostics[0], "\n") != 2 ||
		!strings.HasSuffix(l.Diagnostics[0], "indented two more.") {
		t.Errorf("diagnostics = %q, want TS5102 with its two continuation lines",
			l.Diagnostics)
	}
}

func TestParse_UnresolvedCompilerResolutions(t *testing.T) {
	const text = `======== Resolving module './generated/removed.js' from '/work/o'brien/input.ts'. ========
Explicitly specified module resolution kind: 'Bundler'.
======== Module name './generated/removed.js' was not resolved. ========
======== Resolving module './generated/removed.js' from '/work/other.ts'. ========
======== Module name './generated/removed.js' was successfully resolved to '/work/generated/removed.ts'. ========
======== Resolving module '#mapped' from '/work/input.ts'. ========
Using 'imports' subpath '#mapped' with target 'package'.
======== Resolving module 'package' from '/work/'. ========
======== Module name '#mapped' was not resolved. ========
======== Resolving type reference directive './generated/types', containing file '/work/input.ts', root directory '/work/node_modules/@types'. ========
======== Type reference directive './generated/types' was not resolved. ========
======== Resolving type reference directive '', containing file '/work/__inferred type names__.ts', root directory '/work/node_modules/@types'. ========
======== Type reference directive '' was not resolved. ========
error TS2688: Cannot find type definition file for ''.
input.ts(1,22): error TS6053: File './generated/globals.d.ts' not found.
input.ts(2,22): error TS6231: Could not resolve the path 'generated/absent' with the extensions: '.ts', '.tsx', '.d.ts', '.cts', '.d.cts', '.mts', '.d.mts'.
input.ts(2,1): error TS2322: Type 'string' is not assignable to type 'number'.
input.ts
   Part of 'files' list in tsconfig.json
`
	l, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Unresolved{
		{From: "/work/o'brien/input.ts", Specifier: "./generated/removed.js"},
		{From: "/work/input.ts", Specifier: "#mapped"},
		{Kind: TypeReference, From: "/work/input.ts", Specifier: "./generated/types"},
		{Kind: TypeReference, From: "/work/__inferred type names__.ts", Specifier: ""},
		{Kind: Reference, From: "input.ts", Specifier: "./generated/globals.d.ts", Candidates: []FailedLookup{{File: "./generated/globals.d.ts"}}},
		{Kind: Reference, From: "input.ts", Specifier: "generated/absent", Candidates: []FailedLookup{{File: "generated/absent.ts"}, {File: "generated/absent.tsx"}, {File: "generated/absent.d.ts"}}},
	}
	if !reflect.DeepEqual(l.Unresolved, want) || !slices.Equal(l.Files, []string{"input.ts"}) || !slices.Equal(l.Roots, l.Files) || len(l.Diagnostics) != 4 || !slices.Contains(l.Diagnostics, "error TS2688: Cannot find type definition file for ''.") {
		t.Fatalf("compiler outcomes or listing lost: %+v", l)
	}
	if _, err := Parse("======== Resolving module './missing' from '/work/input.ts'. ========\n"); err == nil {
		t.Fatal("accepted an incomplete compiler resolution trace")
	}
}

func TestParse_UnresolvedTypeReferenceKeepsObservedFilenames(t *testing.T) {
	for _, root := range []string{"'/work/node_modules/@types'", "not set"} {
		t.Run(root, func(t *testing.T) {
			text := "======== Resolving type reference directive '../gen/globals', containing file '/work/app/a.ts', root directory " + root + ". ========\n" +
				"Loading module as file / folder, candidate module location '/work/gen/globals', target file types: Declaration.\n" +
				"File '/work/gen/globals.d.ts' does not exist.\n" +
				"File '/work/gen/globals/package.json' does not exist.\n" +
				"File '/work/gen/globals/index.d.ts' does not exist.\n" +
				"======== Type reference directive '../gen/globals' was not resolved. ========\n" +
				"app/a.ts\n   Part of 'files' list in tsconfig.json\n"
			listing, err := Parse(text)
			if err != nil {
				t.Fatal(err)
			}
			want := []Unresolved{{Kind: TypeReference, From: "/work/app/a.ts", Specifier: "../gen/globals", Candidates: []FailedLookup{{File: "/work/gen/globals.d.ts", Candidate: "/work/gen/globals"}, {File: "/work/gen/globals/package.json", Candidate: "/work/gen/globals"}, {File: "/work/gen/globals/index.d.ts", Candidate: "/work/gen/globals"}}}}
			if !reflect.DeepEqual(listing.Unresolved, want) || !slices.Equal(listing.Files, []string{"app/a.ts"}) {
				t.Fatalf("type-reference probes or framing lost: %+v", listing)
			}
			candidates := []ResolutionCandidate{{Kind: TypeReference, From: "/work/app/a.ts", Specifier: "../gen/globals", Path: "/work/gen/globals"}}
			if !reflect.DeepEqual(listing.Candidates, candidates) {
				t.Fatalf("type-reference candidate was lost or reclassified as a module: %+v", listing.Candidates)
			}
		})
	}
}

func TestParse_FailedLookupsDoNotBorrowAnotherFallbackCandidate(t *testing.T) {
	const trace = `======== Resolving module '#value' from '/work/app/consumer.ts'. ========
Trying substitution '../generated/*', candidate module location: '../generated/value'.
File '/work/generated/value.ts' does not exist.
Loading module as file / folder, candidate module location '/work/generated/value', target file types: TypeScript, Declaration.
File '/work/generated/value.d.ts' does not exist.
Trying substitution './authored/*', candidate module location: './authored/value'.
File '/work/app/authored/value.ts' does not exist.
Loading module '#value' from 'node_modules' folder, target file types: TypeScript, Declaration.
File '/work/node_modules/value.d.ts' does not exist.
Trying substitution './retry/*', candidate module location: './retry/value'.
Using 'imports' subpath '#value' with target './package-target.d.ts'.
File '/work/app/package-target.d.ts' does not exist.
======== Module name '#value' was not resolved. ========
======== Resolving module '#next' from '/work/app/consumer.ts'. ========
File '/work/next.ts' does not exist.
======== Module name '#next' was not resolved. ========
`
	listing, err := Parse(trace)
	if err != nil {
		t.Fatal(err)
	}
	want := []Unresolved{
		{From: "/work/app/consumer.ts", Specifier: "#value", Candidates: []FailedLookup{
			{File: "/work/generated/value.ts", Candidate: "../generated/value"},
			{File: "/work/generated/value.d.ts", Candidate: "/work/generated/value"},
			{File: "/work/app/authored/value.ts", Candidate: "./authored/value"},
			{File: "/work/node_modules/value.d.ts"},
			{File: "/work/app/package-target.d.ts"},
		}},
		{From: "/work/app/consumer.ts", Specifier: "#next", Candidates: []FailedLookup{{File: "/work/next.ts"}}},
	}
	if !reflect.DeepEqual(listing.Unresolved, want) {
		t.Fatalf("failed lookup borrowed another candidate: %+v", listing.Unresolved)
	}
}

func TestParse_UnresolvedPackageTargetsKeepObservedFilenames(t *testing.T) {
	const text = `======== Resolving module '#generated' from '/work/app/consumer.ts'. ========
Using 'imports' subpath '#generated' with target './generated/value.d.ts'.
File '/work/app/generated/value.d.ts' does not exist.
======== Module name '#generated' was not resolved. ========
======== Resolving module 'fixture/generated' from '/work/app/consumer.ts'. ========
Using 'exports' subpath './generated' with target './generated/value.d.ts'.
File '/work/app/generated/value.d.ts' does not exist.
======== Module name 'fixture/generated' was not resolved. ========
======== Resolving module '#redirect' from '/work/consumer.ts'. ========
Using 'imports' subpath '#redirect' with target 'dependency'.
======== Resolving module 'dependency' from '/work/'. ========
File '/work/node_modules/dependency/index.d.ts' does not exist.
======== Module name '#redirect' was not resolved. ========
======== Resolving module '#present' from '/work/app/consumer.ts'. ========
File '/work/app/generated/absent.d.ts' does not exist.
======== Module name '#present' was successfully resolved to '/work/app/generated/value.d.ts'. ========
app/consumer.ts
   Part of 'files' list in tsconfig.json
`
	listing, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Unresolved{
		{From: "/work/app/consumer.ts", Specifier: "#generated", Candidates: []FailedLookup{{File: "/work/app/generated/value.d.ts"}}},
		{From: "/work/app/consumer.ts", Specifier: "fixture/generated", Candidates: []FailedLookup{{File: "/work/app/generated/value.d.ts"}}},
		{From: "/work/consumer.ts", Specifier: "#redirect", Candidates: []FailedLookup{{File: "/work/node_modules/dependency/index.d.ts"}}},
	}
	if !reflect.DeepEqual(listing.Unresolved, want) || !slices.Equal(listing.Files, []string{"app/consumer.ts"}) || len(listing.Edges) != 0 {
		t.Fatalf("package probes, enclosing importer or successful outcome lost: %+v", listing)
	}
	wantTargets := []Edge{
		{From: "/work/app/consumer.ts", Specifier: "#generated", To: "/work/app/generated/value.d.ts"},
		{From: "/work/app/consumer.ts", Specifier: "fixture/generated", To: "/work/app/generated/value.d.ts"},
		{From: "/work/consumer.ts", Specifier: "#redirect", To: "/work/node_modules/dependency/index.d.ts"},
	}
	if !reflect.DeepEqual(listing.PackageTargets, wantTargets) {
		t.Fatalf("package targets lost the enclosing importer or retained an unrelated resolution: %+v", listing.PackageTargets)
	}
}

func TestParse_PackageFallbackTargets(t *testing.T) {
	for _, field := range []string{"imports", "exports"} {
		for _, resolved := range []bool{false, true} {
			outcome := "not resolved"
			probe := "File '/work/o'brien/generated/value.d.ts' does not exist.\n"
			if resolved {
				outcome = "successfully resolved to '/work/o'brien/generated/value.d.ts' with Package ID 'fixture/generated/value.d.ts@1.0.0'"
				probe = "File '/work/o'brien/generated/value.d.ts' exists - use it as a name resolution result.\n"
			}
			text := "======== Resolving module '#generated' from '/work/o'brien/consumer.ts'. ========\n" +
				"Module name '#generated', matched pattern '#generated'.\n" +
				"Trying substitution './nonexistent.d.ts', candidate module location: './nonexistent.d.ts'.\n" +
				"File '/work/o'brien/nonexistent.d.ts' does not exist.\n" +
				"Using '" + field + "' subpath '#generated' with target './generated/absent.d.ts'.\n" +
				"File '/work/o'brien/generated/absent.d.ts' does not exist.\n" +
				"Using '" + field + "' subpath '#generated' with target './generated/value.d.ts'.\n" + probe +
				"======== Module name '#generated' was " + outcome + ". ========\n" +
				"======== Resolving module '#paths' from '/work/consumer.ts'. ========\n" +
				"File '/work/generated/absent.d.ts' does not exist.\n" +
				"======== Module name '#paths' was not resolved. ========\n" +
				"======== Resolving module '#present' from '/work/consumer.ts'. ========\n" +
				"======== Module name '#present' was successfully resolved to '/work/generated/present.d.ts'. ========\n" +
				"consumer.ts\n   Part of 'files' list in tsconfig.json\n"
			listing, err := Parse(text)
			if err != nil {
				t.Fatal(err)
			}
			want := []Edge{
				{Kind: Import, From: "/work/o'brien/consumer.ts", Specifier: "#generated", To: "/work/o'brien/generated/absent.d.ts"},
				{Kind: Import, From: "/work/o'brien/consumer.ts", Specifier: "#generated", To: "/work/o'brien/generated/value.d.ts"},
			}
			if !reflect.DeepEqual(listing.PackageTargets, want) || !slices.Equal(listing.Files, []string{"consumer.ts"}) {
				t.Fatalf("%s resolved=%t: package fallback targets include paths probes or lose the compiler target: %+v", field, resolved, listing)
			}
			if resolved {
				wantResolution := Resolution{
					Edge:          want[1],
					Candidates:    []FailedLookup{{File: "/work/o'brien/nonexistent.d.ts", Candidate: "./nonexistent.d.ts"}, {File: "/work/o'brien/generated/absent.d.ts"}},
					Substitutions: []string{"./nonexistent.d.ts"},
					FromPackage:   true,
				}
				if len(listing.Resolutions) != 2 || !reflect.DeepEqual(listing.Resolutions[0], wantResolution) {
					t.Fatalf("package fallback was attributed to the failed paths substitution: %+v", listing.Resolutions)
				}
			}
		}
	}
}

func TestParse_SubstitutionsKeepEnclosingResolutionAndDoNotLeak(t *testing.T) {
	const text = `======== Resolving module 'choice' from '/work/o'brien/consumer.ts'. ========
Trying substitution '../o'brien/*', candidate module location: '../o'brien/choice'.
File '/work/o'brien/choice.ts' does not exist.
Trying substitution './generated/*', candidate module location: './generated/choice'.
File '/work/generated/choice.ts' does not exist.
Trying substitution './types/v2/*', candidate module location: './types/v2/index'.
======== Module name 'choice' was successfully resolved to '/work/generated/types/v2/index.d.ts'. ========
======== Resolving module '#redirect' from '/work/consumer.ts'. ========
Trying substitution './missing', candidate module location: './missing'.
Using 'imports' subpath '#redirect' with target 'dependency'.
======== Resolving module 'dependency' from '/work/'. ========
Trying substitution './nested', candidate module location: './nested'.
======== Module name '#redirect' was successfully resolved to '/work/dependency/index.d.ts'. ========
======== Resolving module 'absent' from '/work/consumer.ts'. ========
Trying substitution './absent', candidate module location: './absent'.
======== Module name 'absent' was not resolved. ========
======== Resolving module 'next' from '/work/consumer.ts'. ========
======== Module name 'next' was successfully resolved to '/work/next.ts'. ========
`
	listing, err := Parse(strings.ReplaceAll(text, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Resolution{
		{
			Edge:          Edge{Kind: Import, From: "/work/o'brien/consumer.ts", Specifier: "choice", To: "/work/generated/types/v2/index.d.ts"},
			Candidates:    []FailedLookup{{File: "/work/o'brien/choice.ts", Candidate: "../o'brien/choice"}, {File: "/work/generated/choice.ts", Candidate: "./generated/choice"}},
			Substitutions: []string{"../o'brien/*", "./generated/*", "./types/v2/*"},
		},
		{
			Edge:          Edge{Kind: Import, From: "/work/consumer.ts", Specifier: "#redirect", To: "/work/dependency/index.d.ts"},
			Substitutions: []string{"./missing", "./nested"},
			FromPackage:   true,
		},
		{Edge: Edge{Kind: Import, From: "/work/consumer.ts", Specifier: "next", To: "/work/next.ts"}},
	}
	if !reflect.DeepEqual(listing.Resolutions, want) {
		t.Fatalf("substitutions lost their enclosing outcome or leaked into the next frame: %+v", listing.Resolutions)
	}
}

func TestParse_SelectedResolutionFilename(t *testing.T) {
	for _, test := range []struct {
		kind   EdgeKind
		name   string
		suffix string
	}{
		{Import, "Module name", ""},
		{Import, "Module name", " with Package ID 'fixture/value.d.ts@1.0.0'"},
		{TypeReference, "Type reference directive", ", primary: true"},
		{TypeReference, "Type reference directive", " with Package ID '@types/fixture/index.d.ts@1.0.0', primary: false"},
	} {
		start := "======== Resolving module 'fixture' from '/work/consumer.ts'. ========\n"
		if test.kind == TypeReference {
			start = "======== Resolving type reference directive 'fixture', containing file '/work/consumer.ts', root directory not set. ========\n"
		}
		line := "======== " + test.name + " 'fixture' was successfully resolved to '/work/o'brien/value.d.ts'" + test.suffix + ". ========"
		listing, err := Parse(start + "File '/work/absent/value.d.ts' does not exist.\n" + line)
		want := []Resolution{{Edge: Edge{Kind: test.kind, From: "/work/consumer.ts", Specifier: "fixture", To: "/work/o'brien/value.d.ts"}, Candidates: []FailedLookup{{File: "/work/absent/value.d.ts"}}}}
		if err != nil || !reflect.DeepEqual(listing.Resolutions, want) {
			t.Fatalf("lost compiler resolution outcome: %s: %+v, %v", line, listing, err)
		}
	}
}

func TestParse_ResolutionCandidatesRetainOrderWithoutBecomingFilesOrEdges(t *testing.T) {
	const trace = `======== Resolving module '#generated/runtime' from '/work/app/consumer.ts'. ========
Loading module as file / folder, candidate module location '/work/first/runtime', target file types: TypeScript, Declaration.
File '/work/first/runtime.ts' does not exist.
Loading module as file / folder, candidate module location '/work/generated/runtime', target file types: TypeScript, Declaration.
======== Module name '#generated/runtime' was successfully resolved to '/work/generated/runtime/index.d.ts'. ========
======== Resolving module '#missing' from '/work/app/consumer.ts'. ========
Loading module as file / folder, candidate module location '/work/absent', target file types: TypeScript.
======== Module name '#missing' was not resolved. ========
app/consumer.ts
   Part of 'files' list in tsconfig.json
error TS2307: missing import
`
	listing, err := Parse(strings.ReplaceAll(trace, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []ResolutionCandidate{
		{Kind: Import, From: "/work/app/consumer.ts", Specifier: "#generated/runtime", Path: "/work/first/runtime"},
		{Kind: Import, From: "/work/app/consumer.ts", Specifier: "#generated/runtime", Path: "/work/generated/runtime"},
		{Kind: Import, From: "/work/app/consumer.ts", Specifier: "#missing", Path: "/work/absent"},
	}
	if !reflect.DeepEqual(listing.Candidates, want) || !slices.Equal(listing.Files, []string{"app/consumer.ts"}) || !slices.Equal(listing.Roots, listing.Files) || len(listing.Edges) != 0 || len(listing.Diagnostics) != 1 {
		t.Fatalf("candidate locations lost order, attribution or separation from listing facts: %+v", listing)
	}
	if len(listing.Resolutions) != 1 || listing.Resolutions[0].To != "/work/generated/runtime/index.d.ts" || len(listing.Unresolved) != 1 || listing.Unresolved[0].Specifier != "#missing" {
		t.Fatalf("candidate observations lost their enclosing outcome: %+v", listing)
	}
}

func TestParse_ResolutionCandidatesRetainImporterAcrossPackageRedirect(t *testing.T) {
	const text = `======== Resolving module '#alias' from '/work/app/source.ts'. ========
Using 'imports' subpath '#alias' with target 'dependency'.
======== Resolving module 'dependency' from '/work/'. ========
Loading module as file / folder, candidate module location '/work/generated/runtime', target file types: TypeScript.
======== Module name '#alias' was not resolved. ========
`
	listing, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []ResolutionCandidate{{Kind: Import, From: "/work/app/source.ts", Specifier: "#alias", Path: "/work/generated/runtime"}}
	if !reflect.DeepEqual(listing.Candidates, want) {
		t.Fatalf("nested redirect replaced the importing source or specifier: %+v", listing.Candidates)
	}
}

func TestParse_ResolutionFramingRejectsLostCandidateAttribution(t *testing.T) {
	const start = "======== Resolving module '#module' from '/work/app/index.ts'. ========\n"
	const end = "======== Module name '#module' was not resolved. ========\n"
	const candidate = "Loading module as file / folder, candidate module location '/work/generated', target file types: TypeScript.\n"
	const redirect = "Using 'imports' subpath '#module' with target 'dependency'.\n"
	const nested = "======== Resolving module 'dependency' from '/work/'. ========\n"
	for name, text := range map[string]string{
		"nested without redirect":     start + start + end + end,
		"unfinished":                  start,
		"mismatched end":              start + strings.ReplaceAll(end, "#module", "#other"),
		"mismatched kind":             start + strings.ReplaceAll(end, "Module name", "Type reference directive"),
		"unframed end":                end,
		"unframed candidate":          candidate,
		"malformed candidate":         start + "Loading module as file / folder, unknown format\n" + end,
		"relative candidate":          start + strings.ReplaceAll(candidate, "/work/generated", "generated") + end,
		"relative importer":           strings.ReplaceAll(start, "/work/app/index.ts", "app/index.ts") + end,
		"unknown boundary":            "======== Unrecognised resolver boundary ========\n",
		"unknown active boundary":     start + "======== Unrecognised resolver boundary ========\n" + end,
		"different redirect target":   start + redirect + strings.ReplaceAll(nested, "dependency", "other") + end,
		"nonadjacent redirect":        start + redirect + "another trace line\n" + nested + end,
		"exports redirect":            start + strings.ReplaceAll(redirect, "imports", "exports") + nested + end,
		"relative redirect directory": start + redirect + strings.ReplaceAll(nested, "/work/", "relative/") + end,
		"nested outcome":              start + redirect + nested + strings.ReplaceAll(end, "#module", "dependency"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(text); err == nil {
				t.Fatal("malformed trace accepted")
			}
		})
	}
}
