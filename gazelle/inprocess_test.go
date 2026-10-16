package typescript

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/microsoft/typescript-go/shim/listing"
)

// releaseTsgo is tsgo behind a link, beside the package.json of a release of version.
func releaseTsgo(t *testing.T, tsgo, version string) string {
	t.Helper()
	dir := t.TempDir()
	link := filepath.Join(dir, filepath.Base(tsgo))
	if err := os.Symlink(tsgo, link); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name": "@typescript/typescript-linux-x64", "version": "` + version + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return link
}

func countTsgoStarts(t *testing.T) *int {
	t.Helper()
	var started int
	saved := startTsgo
	startTsgo = func(cmd *exec.Cmd) error {
		started++
		return saved(cmd)
	}
	t.Cleanup(func() { startTsgo = saved })
	return &started
}

func TestListingInProcess_StartsTsgoOnlyForAnotherVersion(t *testing.T) {
	requireTsgo(t)
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	root := writeTree(t, map[string]string{
		"tsconfig.json":                 `{"compilerOptions": {"module": "esnext", "moduleResolution": "bundler", "strict": true, "types": ["dep"]}}` + "\n",
		"a.ts":                          "import { b } from './b';\nimport { d } from 'dep';\nimport 'missing';\nexport const a = b + d;\n",
		"b.ts":                          "export interface Options { name: string }\nexport const b = 1;\n",
		"aug.ts":                        "export {};\ndeclare module './b' {\n  interface Options { extra: number }\n}\n",
		"node_modules/dep/package.json": `{"name": "dep", "version": "1.0.0", "types": "index.d.ts"}` + "\n",
		"node_modules/dep/index.d.ts":   "export declare const d: number;\n",
	})
	args := []string{"-p", "tsconfig.json", "--noEmit", "--listFilesOnly", "--explainFiles", "--pretty", "false"}
	for _, tc := range []struct {
		name    string
		version string
		want    int
	}{
		{"a release of the linked-in version lists without tsgo", listing.Version(), 0},
		{"another version lists through tsgo", "0.0.1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := releaseTsgo(t, tsgo, tc.version)
			started := countTsgoStarts(t)
			got, err := runListingUncached(context.Background(), root, release, "tsconfig.json", args)
			if err != nil {
				t.Fatal(err)
			}
			if *started != tc.want {
				t.Errorf("the listing started %d tsgo processes, want %d", *started, tc.want)
			}

			saved := listingInProcessMode
			listingInProcessMode = "off"
			t.Cleanup(func() { listingInProcessMode = saved })
			want, err := runListingUncached(context.Background(), root, release, "tsconfig.json", args)
			if err != nil {
				t.Fatal(err)
			}
			if diff := inProcessDifference(got, want); diff != "" {
				t.Errorf("the listing differs from tsgo's:\n%s", diff)
			}
		})
	}
}

// The in-process listing reuses a chunk's buffer once its lines are handled:
// the splitter must keep nothing of a line, or a later chunk rewrites what it kept.
func TestTraceSplitter_KeepsNothingOfALinesBuffer(t *testing.T) {
	requireTsgo(t)
	tree := subpathTree()
	tree["app/tsconfig.json"] = `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","types":[],"resolveJsonModule":true,"paths":{"@app/*":["./*"]}},"include":["*.ts"]}`
	tree["package.json"] = `{"type":"module","imports":{"#lib/*":"./lib/*.ts","#app/*":"./app/*.ts","#dep":"dep"}}`
	tree["node_modules/dep/package.json"] = `{"name":"dep","types":"index.d.ts"}`
	tree["node_modules/dep/index.d.ts"] = "export declare const dep: number;\n"
	tree["app/empty/notes.txt"] = "no package.json here\n"
	tree["app/j.ts"] = "import data from './empty/package.json';\nimport { dep } from '#dep';\nexport const j = [data, dep];\n"
	root := writeTree(t, tree)
	tsgo, err := newProgramStore().binary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(tsgo, "-p", "app/tsconfig.json", "--noEmit", "--listFilesOnly", "--explainFiles", "--pretty", "false", "--traceResolution", "--locale", "en")
	cmd.Dir = root
	out, _ := cmd.Output()
	// Each phrase leads to a field the splitter keeps across lines.
	for _, phrase := range []string{"======== Resolving module", "Using 'imports' subpath '", "' has a '.json' extension - stripping it.", "Trying substitution '"} {
		if !strings.Contains(string(out), phrase) {
			t.Fatalf("tsgo traced no %q:\n%s", phrase, out)
		}
	}
	var wantFacts, gotFacts resolutionFacts
	wantText, wantCandidates, wantErr := splitTrace(root, string(out), &wantFacts)
	split := newTraceSplitter(root, &gotFacts)
	buf := make([]byte, 0, len(out))
	for _, line := range strings.SplitAfter(string(out), "\n") {
		if line == "" {
			continue
		}
		buf = append(buf[:0], line...)
		if err := split.line(unsafe.String(unsafe.SliceData(buf), len(buf))); err != nil {
			t.Fatal(err)
		}
		for i := range buf {
			buf[i] = 'x'
		}
	}
	gotText, gotCandidates, gotErr := split.finish()
	if gotText != wantText || !reflect.DeepEqual(gotCandidates, wantCandidates) || !reflect.DeepEqual(gotFacts, wantFacts) || (gotErr == nil) != (wantErr == nil) {
		t.Errorf("a rewritten line buffer changed what the splitter kept")
	}
}
