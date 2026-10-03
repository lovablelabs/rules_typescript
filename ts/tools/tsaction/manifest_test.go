package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every field a resolver reads names the emitted file; the rest of the
// manifest, its key order included, is what the member wrote.
func TestManifestAsBuilt(t *testing.T) {
	for _, c := range []struct{ shape, manifest, tsx, want string }{
		{
			"exports subpaths, the @lovable/canvas-sdk shape: .ts targets " +
				"become .js, a css target and a null stay, and fields no " +
				"resolver reads are untouched",
			`{
			  "name": "@lovable/canvas-sdk",
			  "private": true,
			  "type": "module",
			  "exports": {
			    "./wire": "./src/wire/index.ts",
			    "./client/styles.css": "./src/client/styles.css",
			    "./internal/*": null
			  },
			  "files": ["src/*"],
			  "scripts": {"typecheck": "tsc --noEmit"}
			}`,
			".js",
			`{"name":"@lovable/canvas-sdk","private":true,"type":"module",` +
				`"exports":{"./wire":"./src/wire/index.js",` +
				`"./client/styles.css":"./src/client/styles.css",` +
				`"./internal/*":null},"files":["src/*"],` +
				`"scripts":{"typecheck":"tsc --noEmit"}}`,
		},
		{
			"conditions in the order the map writes them: types names the " +
				".d.ts, default the .js",
			`{"name":"pulse","exports":{".":{"types":"./entry.ts",` +
				`"default":"./dist/entry.js"}}}`,
			".js",
			`{"name":"pulse","exports":{".":{"types":"./entry.d.ts",` +
				`"default":"./dist/entry.js"}},"type":"module"}`,
		},
		{
			"the same conditions written the other way round keep that order",
			`{"name":"pulse","exports":{".":{"default":"./entry.ts",` +
				`"types":"./entry.ts"}}}`,
			".js",
			`{"name":"pulse","exports":{".":{"default":"./entry.js",` +
				`"types":"./entry.d.ts"}},"type":"module"}`,
		},
		{
			"a fallback array and a wildcard target",
			`{"name":"m","exports":{".":["./first.ts","./second.js"],` +
				`"./tokens/*":"./styles/tokens/*.ts"}}`,
			".js",
			`{"name":"m","exports":{".":["./first.js","./second.js"],` +
				`"./tokens/*":"./styles/tokens/*.js"},"type":"module"}`,
		},
		{
			"exports as a bare string",
			`{"name":"ws-linked","exports":"./index.ts"}`,
			".js",
			`{"name":"ws-linked","exports":"./index.js","type":"module"}`,
		},
		{
			"main, module and browser name the .js; types and typings the .d.ts",
			`{"name":"m","main":"./src/index.ts","module":"src/index.ts",` +
				`"browser":"./src/browser.tsx","types":"./src/index.ts",` +
				`"typings":"src/legacy.tsx"}`,
			".js",
			`{"name":"m","main":"./src/index.js","module":"src/index.js",` +
				`"browser":"./src/browser.js","types":"./src/index.d.ts",` +
				`"typings":"src/legacy.d.ts","type":"module"}`,
		},
		{
			"imports, the package's own #-specifiers",
			`{"name":"m","imports":{"#internal/*":"./src/internal/*.ts",` +
				`"#dep":"zod"}}`,
			".js",
			`{"name":"m","imports":{"#internal/*":"./src/internal/*.js",` +
				`"#dep":"zod"},"type":"module"}`,
		},
		{
			"local URL paths retain query fragment and encoding while package imports stay external",
			`{"imports":{"#url":"./value%20one.ts?instance=one#part","#pattern":"./parts/*.ts?name=*#part","#literal":"./literal%2A.ts","#external":"other/file.ts","#url-dep":"https://example.test/file.ts"},"exports":{"types":"./value%20one.ts?type#part","default":"./value%20one.ts?instance=one#part"}}`,
			".js",
			`{"imports":{"#url":"./value%20one.js?instance=one#part","#pattern":"./parts/*.js?name=*#part","#literal":"./literal%2A.js","#external":"other/file.ts","#url-dep":"https://example.test/file.ts"},"exports":{"types":"./value%20one.d.ts?type#part","default":"./value%20one.js?instance=one#part"},"type":"module"}`,
		},
		{
			"legacy fields retain filename semantics and encoded URL delimiters stay in the pathname",
			`{"main":"./literal?name.ts","module":"literal#name.ts","browser":"./literal%20name.ts","types":"literal?name.ts","imports":{"#literal":"./literal%3Fname%23part.ts?query#fragment"}}`,
			".js",
			`{"main":"./literal?name.js","module":"literal#name.js","browser":"./literal%20name.js","types":"literal?name.d.ts","imports":{"#literal":"./literal%3Fname%23part.js?query#fragment"},"type":"module"}`,
		},
		{
			".mts and .cts keep their module format in both roles",
			`{"name":"m","main":"./a.cts","module":"./b.mts","types":"./a.cts",` +
				`"exports":{".":{"types":"./b.mts","import":"./b.mts"}}}`,
			".js",
			`{"name":"m","main":"./a.cjs","module":"./b.mjs","types":"./a.d.cts",` +
				`"exports":{".":{"types":"./b.d.mts","import":"./b.mjs"}},` +
				`"type":"module"}`,
		},
		{
			"a declaration target is already the emitted file",
			`{"name":"m","types":"./dist/index.d.ts","exports":{".":{"types":` +
				`"./dist/index.d.mts","default":"./dist/index.d.cts"}}}`,
			".js",
			`{"name":"m","types":"./dist/index.d.ts","exports":{".":{"types":` +
				`"./dist/index.d.mts","default":"./dist/index.d.cts"}},` +
				`"type":"module"}`,
		},
		{
			"type is kept when the member sets it",
			`{"name":"m","type":"commonjs","main":"./index.ts"}`,
			".js",
			`{"name":"m","type":"commonjs","main":"./index.js"}`,
		},
		{
			"scalars survive the round trip",
			`{"name":"m","version":"0.0.0","sideEffects":false,` +
				`"engines":{"node":22}}`,
			".js",
			`{"name":"m","version":"0.0.0","sideEffects":false,` +
				`"engines":{"node":22},"type":"module"}`,
		},
		{
			"under jsx: preserve a .tsx target names the .jsx, a .ts the .js " +
				"and types the .d.ts, in every role and through a wildcard",
			`{"name":"preserve-view","browser":"./browser.tsx","exports":{".":` +
				`"./view.tsx","./icons/*":"./icons/components/*.tsx",` +
				`"./util":{"types":"./util.tsx","default":"./util.ts"}}}`,
			".jsx",
			`{"name":"preserve-view","browser":"./browser.jsx","exports":{".":` +
				`"./view.jsx","./icons/*":"./icons/components/*.jsx",` +
				`"./util":{"types":"./util.d.ts","default":"./util.js"}},` +
				`"type":"module"}`,
		},
		{
			"a nested scope manifest: type kept, nothing else to rewrite",
			`{"type": "module"}`,
			".js",
			`{"type":"module"}`,
		},
	} {
		got, err := manifestAsBuilt([]byte(c.manifest), c.tsx)
		if err != nil {
			t.Errorf("%s: %v", c.shape, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.shape, got, c.want)
		}
	}
}

func TestManifestAsBuilt_RefusesANonObject(t *testing.T) {
	for _, text := range []string{`"main"`, `["./a.ts"]`, `{"name": "m"`} {
		if _, err := manifestAsBuilt([]byte(text), ".js"); err == nil {
			t.Errorf("manifestAsBuilt(%s) = nil error, want one", text)
		}
	}
}

// The step reads SRC and writes OUT with a trailing newline; -tsx is the
// declared emit of a .tsx.
func TestManifestStep_WritesTheFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "package.json")
	out := filepath.Join(dir, "out", "package.json")
	writeFile(t, src,
		"{\n  \"name\": \"view\",\n  \"exports\": \"./view.tsx\"\n}\n")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runManifest([]string{"-tsx=.jsx", src, out}); err != nil {
		t.Fatalf("runManifest: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"view","exports":"./view.jsx","type":"module"}` + "\n"
	if string(data) != want {
		t.Errorf("wrote %q, want %q", data, want)
	}
	if err := runManifest([]string{src}); err == nil ||
		!strings.Contains(err.Error(), "SRC and OUT") {
		t.Errorf("runManifest with one argument = %v, want the usage", err)
	}
}

func TestRuntimeScopeTargetsCannotNameUnpublishedSourceRepresentations(t *testing.T) {
	for _, c := range []struct {
		name, source, want, wantErr string
		targets                     []runtimeTarget
	}{
		{
			name:    "private import and self export use the emitted module",
			source:  `{"name":"borrowed","type":"module","imports":{"#value":"./value.ts"},"exports":{"types":"./value.ts","default":"./value.ts"}}`,
			want:    `{"name":"borrowed","type":"module","imports":{"#value":"./value.js"},"exports":{"types":"./value.ts","default":"./value.js"}}`,
			targets: []runtimeTarget{{"./value.ts", "./value.js"}},
		},
		{
			name:    "source aliases and native JavaScript retain authored targets and format",
			source:  `{"imports":{"#source":"./value.ts","#native":"./legacy.js"}}`,
			want:    `{"imports":{"#source":"./value.ts","#native":"./legacy.js"}}`,
			targets: []runtimeTarget{{"./value.ts", "./value.ts"}, {"./legacy.js", "./legacy.js"}},
		},
		{
			name:    "explicit mixed targets preserve native TypeScript beside emitted modules",
			source:  `{"type":"commonjs","main":"built.ts","imports":{"#dep":"built.ts","#built":"./built.ts","#raw":"./raw.ts","#native":"./legacy.js","#unpublished":"./other.ts"}}`,
			want:    `{"type":"commonjs","main":"built.js","imports":{"#dep":"built.ts","#built":"./built.js","#raw":"./raw.ts","#native":"./legacy.js","#unpublished":"./other.ts"}}`,
			targets: []runtimeTarget{{"./built.ts", "./built.js"}, {"./raw.ts", "./raw.ts"}, {"./legacy.js", "./legacy.js"}},
		},
		{
			name:    "legacy filesystem spellings reach the declared emitted file",
			source:  `{"main":"././built.ts","module":"dir/../built.ts","browser":{"./entry":"./dir/../built.ts"}}`,
			want:    `{"main":"./built.js","module":"built.js","browser":{"./entry":"./built.js"}}`,
			targets: []runtimeTarget{{"./built.ts", "./built.js"}},
		},
		{
			name:    "filesystem targets use exact files and preserve unchanged spellings",
			source:  `{"main":"././raw.ts","module":"dir/../unknown.ts","browser":{"./literal":"./*.ts","./declared-star":"./literal*.ts","./absolute":"/built.ts"}}`,
			want:    `{"main":"././raw.ts","module":"dir/../unknown.ts","browser":{"./literal":"./*.ts","./declared-star":"./literal*.js","./absolute":"/built.ts"}}`,
			targets: []runtimeTarget{{"./raw.ts", "./raw.ts"}, {"./built.ts", "./built.js"}, {"./literal*.ts", "./literal*.js"}},
		},
		{
			name:    "invalid URL dot segments are not normalized into emitted targets",
			source:  `{"exports":{".":"./dir/../built.ts","./dot":"././built.ts"},"imports":{"#bad":"././built.ts"}}`,
			want:    `{"exports":{".":"./dir/../built.ts","./dot":"././built.ts"},"imports":{"#bad":"././built.ts"}}`,
			targets: []runtimeTarget{{"./built.ts", "./built.js"}},
		},
		{
			name:    "wildcards follow all declared matching modules",
			source:  `{"imports":{"#part/*":"./parts/*.ts","#repeated/*":"./*/index/*.ts","#native/*":"./native/*.js"}}`,
			want:    `{"imports":{"#part/*":"./parts/*.js","#repeated/*":"./*/index/*.js","#native/*":"./native/*.js"}}`,
			targets: []runtimeTarget{{"./parts/one.ts", "./parts/one.js"}, {"./parts/two.ts", "./parts/two.js"}, {"./x/index/x.ts", "./x/index/x.js"}, {"./native/a.js", "./native/a.js"}},
		},
		{
			name:    "URL suffixes and encoded pathnames retain module identity",
			source:  `{"imports":{"#first":"./%76alue%20one.ts?instance=one#first","#second":"./value%20one.ts?instance=one#second","#external":"value.ts","#literal":"./literal%2A.ts","#delimiters":"./literal%3Fname%23part.ts?x#y"},"exports":{"types":"./value%20one.ts?type#part","default":"./value%20one.ts?instance=one#first"}}`,
			want:    `{"imports":{"#first":"./value%20one.js?instance=one#first","#second":"./value%20one.js?instance=one#second","#external":"value.ts","#literal":"./literal%2A.js","#delimiters":"./literal%3Fname%23part.js?x#y"},"exports":{"types":"./value%20one.ts?type#part","default":"./value%20one.js?instance=one#first"}}`,
			targets: []runtimeTarget{{"./value one.ts", "./value one.js"}, {"./literal*.ts", "./literal*.js"}, {"./literal?name#part.ts", "./literal?name#part.js"}},
		},
		{
			name:    "wildcard captures exclude query and encoded literal stars",
			source:  `{"imports":{"#part/*":"./parts/%2A/*.ts?instance=*#part"}}`,
			want:    `{"imports":{"#part/*":"./parts/%2A/*.js?instance=*#part"}}`,
			targets: []runtimeTarget{{"./parts/*/one.ts", "./parts/*/one.js"}, {"./parts/*/two.ts", "./parts/*/two.js"}},
		},
		{
			name:    "invalid URL encoding is not published",
			source:  `{"exports":"./value%xy.ts"}`,
			wantErr: "invalid URL escape",
		},
		{
			name:    "encoded separators cannot become valid path separators",
			source:  `{"exports":"./parts%2Fvalue.ts"}`,
			targets: []runtimeTarget{{"./parts/value.ts", "./parts/value.js"}},
			wantErr: "encoded path separator",
		},
		{
			name:    "a wildcard cannot describe incompatible known representations",
			source:  `{"imports":{"#part/*":"./*.ts"}}`,
			targets: []runtimeTarget{{"./built.ts", "./built.js"}, {"./raw.ts", "./raw.ts"}},
			wantErr: "incompatible runtime targets",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			source, out := filepath.Join(root, "source.json"), filepath.Join(root, "runtime.json")
			writeFile(t, source, c.source)
			targets, err := json.Marshal(c.targets)
			if err != nil {
				t.Fatal(err)
			}
			err = runManifest([]string{"-runtime_targets=" + string(targets), source, out})
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want %q", err, c.wantErr)
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatalf("incompatible scope was published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want+"\n" {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestReusedScopeMustPreserveKnownRuntimeTargetsAndFormat(t *testing.T) {
	if err := stamp([]string{"-stamp=" + filepath.Join(t.TempDir(), "empty.check")}); err == nil {
		t.Fatal("stamp accepted no command and no validation obligation")
	}
	for _, c := range []struct{ name, source, runtime, wantErr string }{
		{"compatible partial projection", "", `{"name":"scope","imports":{"#built":"./built.js","#native":"./raw.ts","#other":"./other.js"}}`, ""},
		{"stale source target", "", `{"name":"scope","imports":{"#built":"./built.ts","#native":"./raw.ts"}}`, "needs \"./built.js\""},
		{"blindly projected native target", "", `{"name":"scope","imports":{"#built":"./built.js","#native":"./raw.js"}}`, "needs \"./raw.ts\""},
		{"inserted module format", "", `{"name":"scope","type":"module","imports":{"#built":"./built.js","#native":"./raw.ts","#other":"./other.js"}}`, "changes authored type"},
		{"missing target", "", `{"name":"scope","imports":{"#built":"./built.js"}}`, "omits declared module targets"},
		{"reordered conditions cannot change the selected module", `{"exports":{"node":"./built.ts","default":"./raw.ts"}}`, `{"exports":{"default":"./raw.ts","node":"./built.js"}}`, "changes runtime resolution structure"},
		{"reordered private conditions cannot change the selected module", `{"imports":{"#value":{"node":"./built.ts","default":"./raw.ts"}}}`, `{"imports":{"#value":{"default":"./raw.ts","node":"./built.js"}}}`, "changes runtime resolution structure"},
		{"added exact export cannot shadow the projected wildcard", `{"exports":{"./*":"./b*.ts"}}`, `{"exports":{"./*":"./b*.js","./uilt":"./raw.ts"}}`, "changes runtime resolution structure"},
		{"added null export cannot block the projected wildcard", `{"exports":{"./*":"./b*.ts"}}`, `{"exports":{"./*":"./b*.js","./uilt":null}}`, "changes runtime resolution structure"},
		{"array positions cannot become object conditions", `{"exports":["./built.ts"]}`, `{"exports":{"0":"./built.js"}}`, "changes runtime resolution structure"},
		{"removed null branch cannot expose a blocked subpath", `{"exports":{".":"./built.ts","./blocked":null}}`, `{"exports":{".":"./built.js"}}`, "changes runtime resolution structure"},
		{"browser exclusion cannot change boolean value", `{"browser":{"./built":"./built.ts","./server":false}}`, `{"browser":{"./built":"./built.js","./server":true}}`, "changes runtime resolution structure"},
		{"stale browser exclusion key cannot expose an emitted module", `{"browser":{"./built.ts":false}}`, `{"browser":{"./built.ts":false}}`, "changes runtime resolution structure"},
		{"duplicate runtime browser keys cannot replace an exclusion", `{"browser":{"./built.ts":false}}`, `{"browser":{"./built.js":false,"./built.js":false}}`, "browser key collision"},
		{"subpath and import dictionary order does not reject compatible targets", `{"exports":{".":"./built.ts","./raw":"./raw.ts"},"imports":{"#built":"./built.ts","#raw":"./raw.ts"}}`, `{"imports":{"#raw":"./raw.ts","#built":"./built.js"},"exports":{"./raw":"./raw.ts",".":"./built.js"}}`, ""},
		{"published types and metadata do not reject compatible runtime targets", `{"name":"scope","types":"./built.ts","exports":{"types":"./built.ts","default":"./built.ts"},"description":"authored"}`, `{"description":"published","exports":{"types":"./built.d.ts","default":"./built.js"},"name":"scope","types":"./built.d.ts"}`, ""},
		{"another owner may project an unconstrained conditional target", `{"exports":{"custom":"./other.ts","node":"./built.ts"}}`, `{"exports":{"custom":"./other.js","node":"./built.js"}}`, ""},
		{"stale dotted filesystem target", `{"main":"././built.ts"}`, `{"main":"././built.ts"}`, "needs \"./built.js\""},
		{"stale bare filesystem target", `{"main":"dir/../built.ts"}`, `{"main":"built.ts"}`, "needs \"built.js\""},
		{"equivalent filesystem runtime paths", `{"main":"././built.ts","module":"dir/../built.ts","browser":{"./entry":"./dir/../built.ts"}}`, `{"main":"dir/../built.js","module":"././built.js","browser":{"./entry":"built.js"}}`, ""},
		{"equivalent native filesystem paths", `{"main":"././raw.ts"}`, `{"main":"dir/../raw.ts"}`, ""},
		{"query and fragment retained", `{"name":"scope","exports":"./%62uilt.ts?instance=one#part"}`, `{"name":"scope","exports":"./built.js?instance=one#part"}`, ""},
		{"equivalent encoded runtime pathname", `{"name":"scope","exports":"./built.ts?instance=one#part"}`, `{"name":"scope","exports":"./%62uilt.js?instance=one#part"}`, ""},
		{"stale URL target", `{"name":"scope","exports":"./built.ts?instance=one#part"}`, `{"name":"scope","exports":"./built.ts?instance=one#part"}`, "needs \"./built.js?instance=one#part\""},
		{"lost fragment identity", `{"name":"scope","exports":"./built.ts?instance=one#part"}`, `{"name":"scope","exports":"./built.js?instance=one"}`, "needs \"./built.js?instance=one#part\""},
		{"changed URL identity", `{"name":"scope","exports":"./built.ts?instance=one#part"}`, `{"name":"scope","exports":"./built.js?instance=two#part"}`, "needs \"./built.js?instance=one#part\""},
		{"stale wildcard URL target", `{"name":"scope","imports":{"#part/*":"./b*.ts?instance=*#part"}}`, `{"name":"scope","imports":{"#part/*":"./b*.ts?instance=*#part"}}`, "needs \"./b*.js?instance=*#part\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			check := runtimeScopeCheck{Source: filepath.Join(root, "source.json"), Runtime: filepath.Join(root, "runtime.json"), Targets: []runtimeTarget{{"./built.ts", "./built.js"}, {"./raw.ts", "./raw.ts"}}}
			source := c.source
			if source == "" {
				source = `{"name":"scope","imports":{"#built":"./built.ts","#native":"./raw.ts","#other":"./other.ts"}}`
			}
			writeFile(t, check.Source, source)
			writeFile(t, check.Runtime, c.runtime)
			encoded, err := json.Marshal(check)
			if err != nil {
				t.Fatal(err)
			}
			stampPath := filepath.Join(root, "scope.check")
			err = stamp([]string{"-stamp=" + stampPath, "-runtime_scope=" + string(encoded)})
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(stampPath); err != nil {
					t.Fatalf("validated scope did not publish its stamp: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want %q", err, c.wantErr)
				}
				if _, err := os.Stat(stampPath); !os.IsNotExist(err) {
					t.Fatalf("invalid scope published a validation stamp: %v", err)
				}
			}
		})
	}
}

func TestPublicationTargetsFollowExactMixedRootOutputs(t *testing.T) {
	root := t.TempDir()
	source, output := filepath.Join(root, "package.json"), filepath.Join(root, "built.json")
	body := `{"name":"app","type":"module","main":"index.js","exports":{".":{"types":"./index.d.ts","default":"./index.ts"},"./parts/*":"./parts/*.js","./asset":"./asset.json"},"imports":{"#value":"./index.js"},"browser":{"./index.js":"./browser.js","./server.js":false,"dependency":"./browser.js","index.js":false}}`
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := []runtimeTarget{{"./index.ts", "./app/index.js"}, {"./index.js", "./app/index.js"}, {"./parts/one.js", "./app/parts/one.js"}, {"./parts/two.js", "./app/parts/two.js"}, {"./asset.json", "./app/asset.json"}, {"./browser.js", "./app/browser.js"}, {"./server.js", "./app/server.js"}}
	declarations := []runtimeTarget{{"./index.ts", "./app/index.d.ts"}, {"./index.d.ts", "./app/index.d.ts"}}
	runtimeJSON, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	declarationsJSON, err := json.Marshal(declarations)
	if err != nil {
		t.Fatal(err)
	}
	if err := runManifest([]string{"-runtime_targets=" + string(runtimeJSON), "-declaration_targets=" + string(declarationsJSON), source, output}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"app","type":"module","main":"app/index.js","exports":{".":{"types":"./app/index.d.ts","default":"./app/index.js"},"./parts/*":"./app/parts/*.js","./asset":"./app/asset.json"},"imports":{"#value":"./app/index.js"},"browser":{"./app/index.js":"./app/browser.js","./app/server.js":false,"dependency":"./app/browser.js","index.js":false}}`
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("publication = %s; want %s", got, want)
	}
	for _, publication := range []bool{false, true} {
		t.Run(fmt.Sprintf("browser lookup survives moved namespace/publication=%v", publication), func(t *testing.T) {
			args := []string{"-runtime_targets=" + string(runtimeJSON)}
			if publication {
				args = append(args, "-declaration_targets="+string(declarationsJSON))
			}
			if err := runManifest(append(args, source, output)); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Main    string         `json:"main"`
				Browser map[string]any `json:"browser"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if selected := manifest.Browser["./"+strings.TrimPrefix(manifest.Main, "./")]; selected != "./app/browser.js" {
				t.Fatalf("browser main lookup selects %v instead of the published browser module", selected)
			}
			for key, want := range map[string]any{"./app/server.js": false, "dependency": "./app/browser.js", "index.js": false} {
				if got, exists := manifest.Browser[key]; !exists || got != want {
					t.Fatalf("browser lookup %q = %v, present=%v; want %v", key, got, exists, want)
				}
			}
			if err := validateRuntimeScope(runtimeScopeCheck{Source: source, Runtime: output, Targets: runtime}); err != nil {
				t.Fatalf("scope validation rejected the browser projection: %v", err)
			}
		})
	}
	for _, body := range []string{
		`{"browser":{"./index.js":"./browser.js","./app/index.js":false}}`,
		`{"browser":{"./app/index.js":false,"./index.js":"./browser.js"}}`,
	} {
		t.Run("projected browser key collision cannot replace a mapping", func(t *testing.T) {
			source, output := filepath.Join(t.TempDir(), "source.json"), filepath.Join(t.TempDir(), "runtime.json")
			writeFile(t, source, body)
			for _, publication := range []bool{false, true} {
				args := []string{"-runtime_targets=" + string(runtimeJSON)}
				if publication {
					args = append(args, "-declaration_targets="+string(declarationsJSON))
				}
				if err := runManifest(append(args, source, output)); err == nil || !strings.Contains(err.Error(), "browser key collision") {
					t.Fatalf("publication=%v accepted projected collision: %v", publication, err)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("colliding browser mappings were published: %v", err)
				}
			}
			writeFile(t, output, `{"browser":{"./app/index.js":false}}`)
			if err := validateRuntimeScope(runtimeScopeCheck{Source: source, Runtime: output, Targets: runtime}); err == nil || !strings.Contains(err.Error(), "browser key collision") {
				t.Fatalf("scope validation accepted projected collision: %v", err)
			}
		})
	}
}
