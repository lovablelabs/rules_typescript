package typescript

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

func TestResolutionTraceKeepsCandidatesOutOfListedFilesAndEdges(t *testing.T) {
	requireTsgo(t)
	for _, test := range []struct{ module, chosen, suffixes string }{
		{"value.js", "value.ts", ""},
		{"value.js", "value.native.ts", `,"moduleSuffixes":[".native"]`},
		{"value.mjs", "value.d.mts", ""},
	} {
		t.Run(test.chosen, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"app/tsconfig.json": `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler"` + test.suffixes + `},"files":["index.ts"]}`,
				"app/index.ts":      "export type { Value } from '../generated/missing/" + test.module + "';\n",
			})
			tsgo, err := newProgramStore().binary()
			if err != nil {
				t.Fatal(err)
			}
			var emptyProbes []string
			for _, state := range []string{"cold", "empty", "present"} {
				if state == "empty" {
					if err := os.MkdirAll(filepath.Join(root, "generated/missing"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if state == "present" {
					writeFile(t, filepath.Join(root, "generated/missing", test.chosen), "export type Value = string;\n")
				}
				p, err := listProgram(root, "app", tsgo)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s: candidates=%+v edges=%+v", state, p.candidates, p.Edges)
				var files []string
				for _, candidate := range p.candidates {
					if candidate.file && strings.HasPrefix(candidate.path, "generated/missing/") {
						files = append(files, candidate.path)
					}
				}
				if state == "cold" && len(files) != 0 {
					t.Fatalf("invented cold file lookups: %v", files)
				}

				if state == "empty" && !slices.Contains(files, "generated/missing/"+test.chosen) {
					t.Fatalf("missing compiler probe: %v", files)
				}
				if state == "empty" {
					emptyProbes = slices.Clone(files)
				}
				if state == "present" {
					if at := slices.Index(emptyProbes, "generated/missing/"+test.chosen); at < 0 || !slices.Equal(files, emptyProbes[:at+1]) {
						t.Fatalf("present compiler lookups %v differ from empty probes %v", files, emptyProbes)
					}
					want := []explainfiles.Edge{importEdge("app/index.ts", "../generated/missing/"+test.module, "generated/missing/"+test.chosen)}
					if !slices.Equal(p.Edges, want) {
						t.Fatalf("present listing edges = %+v, want %+v", p.Edges, want)
					}
				} else if len(p.Edges) != 0 {
					t.Fatalf("unresolved candidates became listing edges: %+v", p.Edges)
				}
			}
		})
	}
}

func TestResolutionTraceOrdersFileProbesBeforeDirectoryAndFallback(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name, probes, resolved string
		want                   []resolutionCandidate
	}{
		{
			name: "successful sibling file",
			probes: "Loading module as file / folder, candidate module location '$ROOT/app/generated', target file types: TypeScript, Declaration.\n" +
				"File '$ROOT/app/generated.ts' exists - use it as a name resolution result.\n",
			resolved: "app/generated.ts",
			want:     []resolutionCandidate{{path: "app/generated.ts", file: true}},
		},
		{
			name: "directory before later fallback",
			probes: "Loading module as file / folder, candidate module location '$ROOT/app/generated', target file types: TypeScript, Declaration.\n" +
				"File '$ROOT/app/generated.ts' does not exist.\n" +
				"File '$ROOT/app/generated.tsx' does not exist.\n" +
				"File '$ROOT/app/generated.d.ts' does not exist.\n" +
				"Directory '$ROOT/app/generated' does not exist, skipping all lookups in it.\n" +
				"Loading module as file / folder, candidate module location '$ROOT/app/fallback', target file types: TypeScript, Declaration.\n" +
				"File '$ROOT/app/fallback.ts' exists - use it as a name resolution result.\n",
			resolved: "app/fallback.ts",
			want: []resolutionCandidate{
				{path: "app/generated.ts", file: true},
				{path: "app/generated.tsx", file: true},
				{path: "app/generated.d.ts", file: true},
				{path: "app/generated", skipped: "app/generated"},
				{path: "app/fallback.ts", file: true},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := "======== Resolving module '#value' from '$ROOT/app/index.ts'. ========\n" + test.probes +
				"======== Module name '#value' was successfully resolved to '$ROOT/" + test.resolved + "'. ========\n"
			_, candidates, err := splitResolutionTrace(root, strings.ReplaceAll(trace, "$ROOT", filepath.ToSlash(root)))
			if err != nil {
				t.Fatal(err)
			}
			for i := range test.want {
				test.want[i].from, test.want[i].specifier, test.want[i].resolved = "app/index.ts", "#value", test.resolved
			}
			if !reflect.DeepEqual(candidates, test.want) {
				t.Fatalf("ordered probes = %+v, want %+v", candidates, test.want)
			}
		})
	}
}

func TestResolutionTraceRetainsResolverMetadataAtItsOriginalConsumer(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"module", "type"} {
		for _, probe := range []string{
			"File '$ROOT/lib/package.json' does not exist.\n",
			"File '$ROOT/lib/package.json' does not exist according to earlier cached lookups.\n",
			"Found 'package.json' at '$ROOT/lib/package.json'.\n",
			"File '$ROOT/lib/package.json' exists according to earlier cached lookups.\n",
		} {
			t.Run(kind+"/"+strings.TrimSpace(probe), func(t *testing.T) {
				start := "======== Resolving module '../lib' from '$ROOT/app/index.ts'. ========\n"
				end := "======== Module name '../lib' was successfully resolved to '$ROOT/other/value.ts'. ========\n"
				wantKind := explainfiles.Import
				if kind == "type" {
					start = "======== Resolving type reference directive '../lib', containing file '$ROOT/app/index.ts', root directory '$ROOT/types'. ========\n"
					end = "======== Type reference directive '../lib' was successfully resolved to '$ROOT/other/value.ts', primary: true. ========\n"
					wantKind = explainfiles.TypeReference
				}
				_, candidates, err := splitResolutionTrace(root, strings.ReplaceAll(start+probe+end, "$ROOT", filepath.ToSlash(root)))
				if err != nil {
					t.Fatal(err)
				}
				var metadata []resolutionCandidate
				for _, candidate := range candidates {
					if candidate.metadata {
						metadata = append(metadata, candidate)
					}
				}
				want := []resolutionCandidate{{from: "app/index.ts", specifier: "../lib", path: "lib/package.json", metadata: true, kind: wantKind, resolved: "other/value.ts"}}
				if !reflect.DeepEqual(metadata, want) {
					t.Fatalf("resolver metadata lost importer or redirect attribution: got %+v, want %+v", metadata, want)
				}
			})
		}
	}
}

func TestResolutionTraceKeepsTypeMetadataAcrossCompilerBoundaryForms(t *testing.T) {
	root := t.TempDir()
	for _, start := range []string{
		"containing file '$ROOT/app/index.ts', root directory '$ROOT/types'",
		"containing file '$ROOT/app/index.ts', root directory not set",
		"containing file '$ROOT/app/index.ts'",
		"containing file not set, root directory '$ROOT/types'",
		"containing file not set, root directory not set",
	} {
		for _, end := range []string{
			"was not resolved",
			"was successfully resolved to '$ROOT/lib/index.d.ts', primary: true",
			"was successfully resolved to '$ROOT/lib/index.d.ts' with Package ID 'lib/index.d.ts@1.0.0', primary: false",
		} {
			t.Run(start+"/"+end, func(t *testing.T) {
				trace := "======== Resolving type reference directive 'lib', " + start + ". ========\n" +
					"File '$ROOT/lib.d.ts' does not exist.\n" +
					"File '$ROOT/lib.d.mts' does not exist according to earlier cached lookups.\n" +
					"File '$ROOT/lib.json' does not exist.\n" +
					"Directory '$ROOT/missing' does not exist, skipping all lookups in it.\n" +
					"Found 'package.json' at '$ROOT/lib/package.json'.\n" +
					"======== Type reference directive 'lib' " + end + ". ========\n"
				_, candidates, err := splitResolutionTrace(root, strings.ReplaceAll(trace, "$ROOT", filepath.ToSlash(root)))
				if err != nil {
					t.Fatal(err)
				}
				from, resolved := "", ""
				if strings.Contains(start, "containing file '") {
					from = "app/index.ts"
				}
				if end != "was not resolved" {
					resolved = "lib/index.d.ts"
				}
				want := []resolutionCandidate{
					{from: from, specifier: "lib", path: "lib.d.ts", file: true, kind: explainfiles.TypeReference, resolved: resolved},
					{from: from, specifier: "lib", path: "lib.d.mts", file: true, kind: explainfiles.TypeReference, resolved: resolved},
					{from: from, specifier: "lib", path: "lib/package.json", metadata: true, kind: explainfiles.TypeReference, resolved: resolved},
				}
				if resolved != "" {
					want = append(want, resolutionCandidate{from: from, specifier: "lib", path: resolved, file: true, kind: explainfiles.TypeReference, resolved: resolved})
				}
				if !reflect.DeepEqual(candidates, want) {
					t.Fatalf("type metadata = %+v, want %+v", candidates, want)
				}
			})
		}
	}
}

func TestResolutionTraceRejectsLostCandidateAttribution(t *testing.T) {
	root := t.TempDir()
	start := "======== Resolving module '#module' from '" + filepath.Join(root, "app/index.ts") + "'. ========\n"
	end := "======== Module name '#module' was not resolved. ========\n"
	for name, text := range map[string]string{
		"nested":              start + start + end + end,
		"unfinished":          start,
		"mismatched end":      start + strings.ReplaceAll(end, "#module", "#other"),
		"unframed candidate":  "Loading module as file / folder, candidate module location '/app/generated', target file types: TypeScript.\n",
		"malformed candidate": start + "Loading module as file / folder, unknown format\n" + end,
		"relative candidate":  start + "Loading module as file / folder, candidate module location 'generated', target file types: TypeScript.\n" + end,
		"unknown boundary":    "======== Unrecognised resolver boundary ========\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := splitResolutionTrace(root, text); err == nil {
				t.Fatal("malformed trace accepted")
			}
		})
	}
}

func TestResolutionTracePreservesActualCompilerListing(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"package.json":                         `{"name":"fixture","imports":{"#redirect":"dependency"}}`,
		"node_modules/dependency/package.json": `{"name":"dependency","types":"index.d.ts"}`,
		"node_modules/dependency/index.d.ts":   "export declare const dependency: string;\n",
		"app/tsconfig.json":                    `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","paths":{"#shared/*":["./shared/*"]}},"include":["shared/**/*.ts"]}`,
		"app/shared/value.ts":                  "export const value = 1;\n",
		"app/plugins/tsconfig.json":            `{"extends":"../tsconfig.json","include":["*.ts"]}`,
		"app/plugins/consumer.ts":              "import { dependency } from '#redirect';\nimport { value } from '#shared/value';\nimport { generated } from '#shared/generated/runtime';\nexport const result = [value, generated];\n",
	})
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(tsgo, "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("selected compiler %s: %s", tsgo, version)
	var baseline *explainfiles.Listing
	for _, traced := range []bool{false, true} {
		args := []string{"-p", "app/plugins/tsconfig.json", "--noEmit", "--listFilesOnly", "--explainFiles", "--pretty", "false", "--locale", "en"}
		if traced {
			args = append(args, "--traceResolution")
		}
		cmd := exec.Command(tsgo, args...)
		cmd.Dir = root
		started := time.Now()
		raw, err := cmd.CombinedOutput()
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("compiler listing: %v\n%s", err, raw)
		}
		t.Logf("trace=%t elapsed=%s bytes=%d argv=%q; same cold-tree fixture, fresh process, untraced first, filesystem cache uncontrolled", traced, elapsed, len(raw), args)
		name := "plain"
		if traced {
			name = "trace"
		}
		if out := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); out != "" {
			if err := os.WriteFile(filepath.Join(out, "generated-tree-"+name+".log"), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		text := string(raw)
		if traced {
			if !strings.Contains(text, "======== Resolving module 'dependency' from '") {
				t.Fatal("package imports continuation was not exercised")
			}
			var candidates []resolutionCandidate
			text, candidates, err = splitResolutionTrace(root, text)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, candidate := range candidates {
				if candidate.from == "app/plugins/consumer.ts" && candidate.specifier == "#shared/generated/runtime" && candidate.path == "app/shared/generated/runtime" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing actual cold-tree candidate: %+v", candidates)
			}
		}
		listing, err := explainfiles.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if !traced {
			baseline = listing
		} else if !reflect.DeepEqual(listing, baseline) {
			t.Fatalf("trace changed compiler listing: got %+v, want %+v", listing, baseline)
		}
	}
}

func TestResolutionTraceKeepsOriginalImporterAcrossPackageRedirect(t *testing.T) {
	root := t.TempDir()
	text := "======== Resolving module '#alias' from '" + filepath.Join(root, "app/source.ts") + "'. ========\n" +
		"Using 'imports' subpath '#alias' with target 'dependency'.\n" +
		"======== Resolving module 'dependency' from '" + root + "/'. ========\n" +
		"Loading module as file / folder, candidate module location '" + filepath.Join(root, "generated/runtime") + "', target file types: TypeScript.\n" +
		"Directory '" + filepath.Join(root, "generated") + "' does not exist, skipping all lookups in it.\n" +
		"======== Module name '#alias' was not resolved. ========\n"
	_, candidates, err := splitResolutionTrace(root, text)
	if err != nil {
		t.Fatal(err)
	}
	want := []resolutionCandidate{{from: "app/source.ts", specifier: "#alias", path: "generated/runtime", skipped: "generated"}}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("candidates = %+v, want %+v", candidates, want)
	}
}
