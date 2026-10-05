package typescript

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// The npm shapes: root and nested importers, two declaring nothing, links, an
// alias, an os-bound package, @types alone, a member's name from the registry.
const lockText = `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@acme/lib':
        specifier: workspace:*
        version: link:packages/lib
      vite:
        specifier: 8.2.2
        version: 8.2.2(@types/node@22.20.1)
      zod:
        specifier: ^3.0.0
        version: 3.24.2
    devDependencies:
      '@types/node':
        specifier: 22.20.1
        version: 22.20.1
      typescript:
        specifier: 5.9.2
        version: 5.9.2

  packages/lib:
    dependencies:
      zod:
        specifier: ^3.0.0
        version: 3.24.2

  packages/ui: {}

  web:
    dependencies:
      '@acme/lib':
        specifier: ^1.0.0
        version: 1.0.0
      '@acme/ui':
        specifier: workspace:*
        version: link:../packages/ui
      '@acme/vendored':
        specifier: file:../third_party/acme-vendored-1.0.0.tgz
        version: file:third_party/acme-vendored-1.0.0.tgz
      marked:
        specifier: ^15.0.0
        version: 15.0.12
      react:
        specifier: ^19.0.0
        version: 19.0.0
    devDependencies:
      '@types/mdast':
        specifier: ^4.0.0
        version: 4.0.4
      '@types/react':
        specifier: ^19.0.0
        version: 19.0.0
      tailwindcss-v3:
        specifier: npm:tailwindcss@3.4.0
        version: tailwindcss@3.4.0

  workers/api-gateway: {}

  workers/download:
    dependencies:
      api-gateway:
        specifier: 1.0.0
        version: 1.0.0
    devDependencies:
      typescript:
        specifier: 5.9.2
        version: 5.9.2
      wrangler:
        specifier: 4.118.0
        version: 4.118.0

packages:

  '@acme/lib@1.0.0':
    resolution: {integrity: sha512-lll}

  '@acme/vendored@file:third_party/acme-vendored-1.0.0.tgz':
    resolution: {integrity: sha512-vvv, tarball: file:third_party/acme-vendored-1.0.0.tgz}
    version: 1.0.0

  '@types/mdast@4.0.4':
    resolution: {integrity: sha512-mmm}

  '@types/node@22.20.1':
    resolution: {integrity: sha512-aaa}

  '@types/react@19.0.0':
    resolution: {integrity: sha512-bbb}

  api-gateway@1.0.0: {}

  fsevents@2.3.3:
    resolution: {integrity: sha512-ccc}
    os: [darwin]

  marked@15.0.12: {}

  marked@17.0.1: {}

  react@19.0.0: {}

  tailwindcss@3.4.0: {}

  typescript@5.9.2: {}

  vite@8.2.2: {}

  wrangler@4.118.0: {}

  zod@3.24.2: {}

snapshots:

  '@acme/lib@1.0.0': {}

  '@acme/vendored@file:third_party/acme-vendored-1.0.0.tgz': {}

  '@types/mdast@4.0.4': {}

  '@types/node@22.20.1': {}

  '@types/react@19.0.0': {}

  api-gateway@1.0.0: {}

  fsevents@2.3.3:
    optional: true

  marked@15.0.12: {}

  marked@17.0.1: {}

  react@19.0.0: {}

  tailwindcss@3.4.0: {}

  typescript@5.9.2: {}

  vite@8.2.2:
    optionalDependencies:
      fsevents: 2.3.3

  wrangler@4.118.0: {}

  zod@3.24.2: {}
`

const (
	store           = "../../../.local/share/pnpm/store/v11/links/"
	storeTypesReact = store + "@types/react/19.0.0/aaa/node_modules/" +
		"@types/react/index.d.ts"
	storeTypesNodeFS = store + "@types/node/22.20.1/bbb/node_modules/" +
		"@types/node/fs.d.ts"
	storeTypesMdast = store + "@types/mdast/4.0.4/kkk/node_modules/" +
		"@types/mdast/index.d.ts"
	storeAcmeLib = store + "@acme/lib/1.0.0/iii/node_modules/@acme/lib/" +
		"wire/index.d.ts"
	storeApiGateway = store + "@/api-gateway/1.0.0/jjj/node_modules/" +
		"api-gateway/index.d.ts"
	storeViteClient = store + "@/vite/8.2.2/ccc/node_modules/vite/client.d.ts"
	storeMarked     = store + "@/marked/15.0.12/ddd/node_modules/marked/" +
		"lib/marked.d.ts"
	storeTailwind = store + "@/tailwindcss/3.4.0/eee/node_modules/" +
		"tailwindcss/types/index.d.ts"
	storeZod = store + "@/zod/3.24.2/fff/node_modules/zod/index.d.ts"
	storeSDK = store + "@anthropic-ai/sdk/0.1.0/ggg/node_modules/" +
		"@anthropic-ai/sdk/index.d.ts"
	storeFsevents = store + "@/fsevents/2.3.3/hhh/node_modules/fsevents/" +
		"fsevents.d.ts"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNpmPackageToLabelName(t *testing.T) {
	for pkg, want := range map[string]string{
		"vitest":           "vitest",
		"@types/react":     "types_react",
		"@tanstack/router": "tanstack_router",
		"@scope/a-b.c":     "scope_a-b.c",
		"lodash.debounce":  "lodash.debounce",
	} {
		if got := npmPackageToLabelName(pkg); got != want {
			t.Errorf("npmPackageToLabelName(%q) = %q, want %q", pkg, got, want)
		}
	}
}

func TestTypesPackage(t *testing.T) {
	for spec, want := range map[string]string{
		"node":        "@types/node",
		"vite/client": "@types/vite",
		"@acme/ui":    "@types/acme__ui",
		"@acme/ui/x":  "@types/acme__ui",
	} {
		if got := typesPackage(spec); got != want {
			t.Errorf("typesPackage(%q) = %q, want %q", spec, got, want)
		}
	}
}

func TestBarePackageName(t *testing.T) {
	for spec, want := range map[string]string{
		"react":                    "react",
		"react/jsx-runtime":        "react",
		"@tanstack/router":         "@tanstack/router",
		"@tanstack/router/history": "@tanstack/router",
		"@scope":                   "@scope",
		"a/b/c":                    "a",
	} {
		if got := barePackageName(spec); got != want {
			t.Errorf("barePackageName(%q) = %q, want %q", spec, got, want)
		}
	}
}

// The workspace on disk. packages/ui is linked as @acme/ui, so its manifest's
// name is no member's; nothing links workers/api-gateway, so its own name is.
func npmRepo(t *testing.T) (string, *npmLock) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, pnpmLockfileName), lockText)
	for dir, name := range map[string]string{
		"":                    "monorepo",
		"packages/lib":        "@acme/lib",
		"packages/ui":         "@acme/ui-src",
		"web":                 "web-app",
		"workers/api-gateway": "api-gateway",
		"workers/download":    "download",
	} {
		writeFile(t, filepath.Join(root, dir, "package.json"),
			`{"name": "`+name+`"}`)
	}
	writeFile(t, filepath.Join(root, "packages/lib/example/package.json"),
		`{"name": "@acme/lib-example"}`)
	l, err := loadNpmLock(root, func(dir string) *manifest { return readManifest(root, dir) })
	if err != nil {
		t.Fatal(err)
	}
	return root, l
}

func TestNpmPackageName(t *testing.T) {
	pnpmStore := "node_modules/.pnpm/react@19.0.0/node_modules/react/index.d.ts"
	for _, c := range []struct{ listed, want string }{
		{store + "@vitest/utils/4.1.5/1961/node_modules/@vitest/utils/dist/" +
			"index.d.ts", "@vitest/utils"},
		{storeTypesReact, "@types/react"},
		{storeZod, "zod"},
		{"node_modules/zod/index.d.ts", "zod"},
		{pnpmStore, "react"},
		{"web/node_modules/@types/culori/index.d.ts", "@types/culori"},
		{"web/src/app.tsx", ""},
		{"node_modules/@types", ""},
	} {
		if got := npmPackageName(c.listed); got != c.want {
			t.Errorf("npmPackageName(%q) = %q, want %q", c.listed, got, c.want)
		}
	}
}

func TestParsePnpmImporters(t *testing.T) {
	got := parsePnpmImporters(strings.Split(lockText, "\n"))
	if dirs := slices.Sorted(maps.Keys(got)); !slices.Equal(dirs, []string{
		"", "packages/lib", "packages/ui", "web", "workers/api-gateway",
		"workers/download",
	}) {
		t.Errorf("importer dirs = %v", dirs)
	}
	if deps := slices.Sorted(maps.Keys(got["web"].deps)); !slices.Equal(deps,
		[]string{"@acme/lib", "@acme/vendored", "@types/mdast", "@types/react",
			"marked", "react", "tailwindcss-v3"}) {
		t.Errorf("web declares %v", deps)
	}
	if got["web"].deps["tailwindcss-v3"] != "tailwindcss@3.4.0" {
		t.Errorf("web's tailwindcss-v3 = %q", got["web"].deps["tailwindcss-v3"])
	}
	// A link: is resolved against the importer that declares it.
	if want := map[string]string{"@acme/ui": "packages/ui"}; !reflect.
		DeepEqual(got["web"].links, want) {
		t.Errorf("web links %v, want %v", got["web"].links, want)
	}
	if want := map[string]string{"@acme/lib": "packages/lib"}; !reflect.
		DeepEqual(got[""].links, want) {
		t.Errorf("root links %v, want %v", got[""].links, want)
	}
	for _, dir := range []string{"packages/ui", "workers/api-gateway"} {
		if n := len(got[dir].deps) + len(got[dir].links); n != 0 {
			t.Errorf("%s declares %d names, want 0", dir, n)
		}
	}
}

// The v6 spelling: the whole entry on the dep's line.
func TestParsePnpmImporters_V6Inline(t *testing.T) {
	const lock = `lockfileVersion: '6.0'

importers:

  .:
    dependencies:
      shared: {specifier: workspace:*, version: link:packages/shared}
      h3-v2: {specifier: npm:h3@2.0.1, version: h3@2.0.1}

  apps/web:
    dependencies:
      zod: {specifier: ^3.0.0, version: 3.24.2}
`
	got := parsePnpmImporters(strings.Split(lock, "\n"))
	if want := map[string]string{"shared": "packages/shared"}; !reflect.
		DeepEqual(got[""].links, want) {
		t.Errorf("root links %v, want %v", got[""].links, want)
	}
	if got[""].deps["h3-v2"] != "h3@2.0.1" {
		t.Errorf("root deps %v", got[""].deps)
	}
	if got["apps/web"].deps["zod"] != "3.24.2" {
		t.Errorf("apps/web deps %v", got["apps/web"].deps)
	}
}

// The refusal set takes every name either section spells, whichever way the
// mapping is written, plus the links and aliases no package key accounts for.
func TestParsePnpmLockNames(t *testing.T) {
	const lock = `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@acme/lib':
        specifier: workspace:*
        version: link:packages/lib
      h3-v2:
        specifier: npm:h3@2.0.1
        version: h3@2.0.1

packages:

  zod@3.24.2: {}

  fsevents@2.3.3:
    resolution: {integrity: sha512-bbb}
    os: [darwin]

snapshots:

  h3@2.0.1: {}
`
	got := parsePnpmLockNames(strings.Split(lock, "\n"))
	want := map[string]bool{
		"zod":       true,
		"fsevents":  true,
		"h3":        true,
		"@acme/lib": true,
		"h3-v2":     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePnpmLockNames = %v, want %v", got, want)
	}
}

// The members are the hub's: every link: name, and the manifest name of every
// importer but the root that no link names, one written as `dir: {}` included.
func TestNpmLock_Members(t *testing.T) {
	_, l := npmRepo(t)
	want := map[string]string{
		"@acme/lib":   "packages/lib",
		"@acme/ui":    "packages/ui",
		"web-app":     "web",
		"api-gateway": "workers/api-gateway",
		"download":    "workers/download",
	}
	if !reflect.DeepEqual(l.members, want) {
		t.Errorf("members = %v, want %v", l.members, want)
	}
	for _, name := range []string{
		"@anthropic-ai/sdk", "@acme/lib-example", "monorepo", "@acme/ui-src",
	} {
		if l.names[name] {
			t.Errorf("%s is in the lockfile's names", name)
		}
	}
	for _, name := range []string{
		"zod", "fsevents", "tailwindcss", "tailwindcss-v3", "@acme/ui",
	} {
		if !l.names[name] {
			t.Errorf("%s is not in the lockfile's names", name)
		}
	}
}

func TestNpmLock_ImporterAbove(t *testing.T) {
	_, l := npmRepo(t)
	for dir, want := range map[string]string{
		"web/src/components":      "web",
		"web":                     "web",
		"packages/lib/src":        "packages/lib",
		"workers/api-gateway/src": "workers/api-gateway",
		"scripts":                 "",
		"":                        "",
	} {
		if got := l.importerAbove(dir); got != want {
			t.Errorf("importerAbove(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A name the importing file's own importer does not declare is spelled under
// the nearest importer above it that does, the root last: pnpm's walk-up.
func TestNpmLock_LabelWalksTheChain(t *testing.T) {
	const lock = `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      zod:
        specifier: ^3.0.0
        version: 3.24.2
    devDependencies:
      typescript:
        specifier: 5.9.2
        version: 5.9.2

  packages/lib:
    dependencies:
      zod:
        specifier: ^4.0.0
        version: 4.1.0

  packages/lib/example: {}

packages:

  typescript@5.9.2: {}

  zod@3.24.2: {}

  zod@4.1.0: {}
`
	root := t.TempDir()
	writeFile(t, filepath.Join(root, pnpmLockfileName), lock)
	for dir, name := range map[string]string{
		"packages/lib":         "@acme/lib",
		"packages/lib/example": "@acme/lib-example",
	} {
		writeFile(t, filepath.Join(root, dir, "package.json"),
			`{"name": "`+name+`"}`)
	}
	l, err := loadNpmLock(root, func(dir string) *manifest { return readManifest(root, dir) })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, dir, want string }{
		{"zod", "packages/lib/example/src", "@npm//packages/lib:zod"},
		{"typescript", "packages/lib/example/src", "@npm//:typescript"},
		{"zod", "packages/lib/src", "@npm//packages/lib:zod"},
		{"zod", "scripts", "@npm//:zod"},
	} {
		if got := l.label(c.name, c.dir); got != c.want {
			t.Errorf("label(%q, %q) = %q, want %q", c.name, c.dir, got, c.want)
		}
	}
}

func importEdge(from, spec, to string) explainfiles.Edge {
	return explainfiles.Edge{Kind: explainfiles.Import, From: from, To: to,
		Specifier: spec}
}

// One label per edge: the specifier's package, spelled for the importer
// whose node_modules resolved it. The @types twin is the hub's pairing.
func TestEdgeLabel_ReactIntoTypesReactIsOneLabel(t *testing.T) {
	root, l := npmRepo(t)
	e := importEdge("web/src/app.tsx", "react", storeTypesReact)
	if got := l.edgeLabel(e, "web", fixtureManifest(root, parentDir(e.From))); got != "@npm//web:react" {
		t.Errorf("edgeLabel = %q, want @npm//web:react", got)
	}
	e = importEdge("web/src/app.tsx", "react/jsx-runtime", storeTypesReact)
	if got := l.edgeLabel(e, "web", fixtureManifest(root, parentDir(e.From))); got != "@npm//web:react" {
		t.Errorf("jsx-runtime edgeLabel = %q, want @npm//web:react", got)
	}
}

// @npm//<importer>:<name> when the nearest importer above the importing file
// declares the name, @npm//:<name> otherwise (D7).
func TestEdgeLabel_ImporterScopedAgainstRoot(t *testing.T) {
	root, l := npmRepo(t)
	for _, c := range []struct{ from, spec, to, want string }{
		{"web/src/markdown.ts", "marked", storeMarked, "@npm//web:marked"},
		{"web/src/schema.ts", "zod", storeZod, "@npm//:zod"},
		{"packages/lib/src/index.ts", "zod", storeZod, "@npm//packages/lib:zod"},
		{"scripts/build.ts", "zod", storeZod, "@npm//:zod"},
		{"workers/api-gateway/src/index.ts", "zod", storeZod, "@npm//:zod"},
		{"workers/download/test/index.spec.ts", "typescript",
			store + "@/typescript/5.9.2/iii/node_modules/typescript/lib/" +
				"typescript.d.ts", "@npm//workers/download:typescript"},
		{"web/src/x.ts", "fsevents", storeFsevents, "@npm//:fsevents"},
		{"web/src/track.ts", "@acme/vendored", store + "@acme/vendored/1.0.0/" +
			"lll/node_modules/@acme/vendored/index.d.ts", "@npm//web:acme_vendored"},
	} {
		e := importEdge(c.from, c.spec, c.to)
		if got := l.edgeLabel(e, parentDir(c.from), fixtureManifest(root, parentDir(e.From))); got != c.want {
			t.Errorf("%s imports %q: %q, want %q", c.from, c.spec, got, c.want)
		}
	}
}

// An alias is the importer's name for the package; the store path carries the
// package's own.
func TestEdgeLabel_AliasKeepsTheImportersName(t *testing.T) {
	root, l := npmRepo(t)
	e := importEdge("web/src/styles.ts", "tailwindcss-v3/plugin", storeTailwind)
	got := l.edgeLabel(e, "web", fixtureManifest(root, parentDir(e.From)))
	if got != "@npm//web:tailwindcss-v3" {
		t.Errorf("edgeLabel = %q, want @npm//web:tailwindcss-v3", got)
	}
}

// An edge with no bare package in its specifier takes the listed file's.
func TestEdgeLabel_NoBarePackageTakesTheListedFile(t *testing.T) {
	root, l := npmRepo(t)
	for _, c := range []struct {
		e    explainfiles.Edge
		want string
	}{
		{explainfiles.Edge{Kind: explainfiles.TypeReference,
			From: "web/src/env.d.ts", To: storeViteClient,
			Specifier: "vite/client"}, "@npm//:vite"},
		{explainfiles.Edge{Kind: explainfiles.TypeReference,
			From: "scripts/run.ts", To: storeNode, Specifier: "node"},
			"@npm//:types_node"},
		{importEdge("scripts/run.ts", "node:fs", storeTypesNodeFS),
			"@npm//:types_node"},
		{importEdge("web/src/x.ts", "#dep", storeZod), "@npm//:zod"},
	} {
		if got := l.edgeLabel(c.e, parentDir(c.e.From), fixtureManifest(root, parentDir(c.e.From))); got != c.want {
			t.Errorf("%q from %s: %q, want %q", c.e.Specifier, c.e.From, got,
				c.want)
		}
	}
}

// A name the lockfile never mentions has no hub target: one log line naming
// the importer and the specifier, and no label.
func TestEdgeLabel_UnknownNameIsRefused(t *testing.T) {
	root, l := npmRepo(t)
	e := importEdge("tools/pr/classification.ts", "@anthropic-ai/sdk/resources",
		storeSDK)
	var got string
	out := captureLog(t, func() { got = l.edgeLabel(e, "tools/pr", fixtureManifest(root, parentDir(e.From))) })
	if got != "" {
		t.Errorf("edgeLabel = %q, want none", got)
	}
	for _, want := range []string{
		"tools/pr/classification.ts", "@anthropic-ai/sdk/resources",
		"@anthropic-ai/sdk", "pnpm-lock.yaml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("%d log lines, want 1: %q", n, out)
	}
}

// A bare import that lands in a @types package installed with no base package
// beside it is that package, the declaring importer's; no line.
func TestEdgeLabel_TypesOnlyInstallationIsTheListedPackage(t *testing.T) {
	root, l := npmRepo(t)
	cases := []struct{ from, spec, to, want string }{
		{"web/src/mention.ts", "mdast", storeTypesMdast, "@npm//web:types_mdast"},
		{"scripts/run.ts", "fs", storeTypesNodeFS, "@npm//:types_node"},
	}
	out := captureLog(t, func() {
		for _, c := range cases {
			e := importEdge(c.from, c.spec, c.to)
			if got := l.edgeLabel(e, parentDir(c.from), fixtureManifest(root, parentDir(e.From))); got != c.want {
				t.Errorf("%s imports %q: %q, want %q", c.from, c.spec, got, c.want)
			}
		}
	})
	if out != "" {
		t.Errorf("log %q, want none", out)
	}
}

// culori publishes no declarations; the import resolves into @types/culori
// and the label is culori's, from the fixture lockfile the hub is built from.
func TestEdgeLabel_CuloriPairing(t *testing.T) {
	data, err := os.ReadFile("../tests/npm/pnpm-lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	l := parseNpmLock(strings.Split(string(data), "\n"), func(dir string) *manifest { return readManifest(root, dir) })
	e := importEdge("tests/npm/app.ts", "culori",
		store+"@types/culori/2.1.1/jjj/node_modules/@types/culori/index.d.ts")
	if got := l.edgeLabel(e, "tests/npm", fixtureManifest(root, parentDir(e.From))); got != "@npm//:culori" {
		t.Errorf("edgeLabel = %q, want @npm//:culori", got)
	}
	if want := map[string]string{
		"shared": "packages/shared", "pulse": "packages/pulse",
		"@acme/ui":       "tests/compiler_options/member/lib",
		"hoisted-member": "packages/hoisted-member",
		"nested-shared":  "packages/nested-shared",
		"preserve-view":  "tests/jsx_preserve/member",
		"by-name-member": "packages/by-name-member",
		"subpath-member": "packages/subpath-member",
	}; !reflect.DeepEqual(l.members, want) {
		t.Errorf("members = %v, want %v", l.members, want)
	}
}

// An augmentation names its module as an import does: the specifier's
// package, here culori's label for a file under its @types twin.
func TestEdgeLabel_AugmentationNamesThePackage(t *testing.T) {
	data, err := os.ReadFile("../tests/npm/pnpm-lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	l := parseNpmLock(strings.Split(string(data), "\n"), func(dir string) *manifest { return readManifest(root, dir) })
	e := explainfiles.Edge{Kind: explainfiles.Augmentation,
		From: "tests/npm/probe.ts", Specifier: "culori",
		To: store + "@types/culori/2.1.1/jjj/node_modules/@types/culori/index.d.ts"}
	if got := l.edgeLabel(e, "tests/npm", fixtureManifest(root, parentDir(e.From))); got != "@npm//:culori" {
		t.Errorf("edgeLabel = %q, want @npm//:culori", got)
	}
}

// A member by name is the nearest importer's resolution of it, a link or a
// registry version; "" and a line for none; a self-reference gets no view.
func TestMemberView(t *testing.T) {
	root, l := npmRepo(t)
	cases := []struct {
		spec, file, pkg, want string
		ok                    bool
	}{
		{"@acme/lib/wire", "packages/app/src/a.ts", "packages/app",
			"//:node_modules/@acme/lib", true},
		{"@acme/lib", "packages/lib/src/x.test.ts", "packages/lib", "", false},
		{"@acme/lib/icons/Check", "packages/lib/src/index.ts", "packages/lib",
			"", false},
		{"@acme/lib/wire", "packages/lib/tools/gen.ts", "packages/lib/tools",
			"", false},
		{"@acme/lib/wire", "packages/lib/example/src/a.ts",
			"packages/lib/example", "//:node_modules/@acme/lib", true},
		{"@acme/lib", "x.test.ts", "", ":node_modules/@acme/lib", true},
		{"@acme/ui", "web/src/a.ts", "web", ":node_modules/@acme/ui", true},
		{"@acme/ui", "web/src/a.ts", "web/src", "//web:node_modules/@acme/ui",
			true},
		{"@acme/lib", "web/src/a.ts", "web", "@npm//web:acme_lib", true},
		{"@acme/lib", "web/vitest.config.mts", "packages/app/test", "@npm//web:acme_lib", true},
		{"@acme/ui", "web/vitest.config.mts", "packages/app/test", "//web:node_modules/@acme/ui", true},
		{"@acme/lib", "packages/app/vitest.config.mts", "web", "//:node_modules/@acme/lib", true},
		{"@acme/ui", "packages/app/src/a.ts", "packages/app", "", false},
		{"web-app", "web/src/a.ts", "web", "", false},
		{"web-app", "web/src/a.test.ts", "web", "", false},
		{"download", "workers/download/test/x.test.ts", "workers/download/test",
			"", false},
		{"api-gateway", "web/src/a.ts", "web", "", false},
		{"api-gateway", "workers/download/src/x.ts", "workers/download",
			"@npm//workers/download:api-gateway", true},
		{"api-gateway", "workers/api-gateway/src/x.ts", "workers/api-gateway",
			"", false},
		{"api-gateway", "workers/api-gateway/test/x.test.ts",
			"workers/api-gateway/test", "", false},
		{"zod", "packages/lib/src/index.ts", "packages/lib", "", false},
		{"./wire", "packages/lib/src/index.ts", "packages/lib", "", false},
		{"@acme/ui-src", "web/src/a.ts", "web", "", false},
	}
	logged := captureLog(t, func() {
		for _, c := range cases {
			got, ok := l.memberView(c.spec, c.file, c.pkg, fixtureManifest(root, parentDir(c.file)))
			if got != c.want || ok != c.ok {
				t.Errorf("%s in %s imports %q: (%q, %v), want (%q, %v)", c.file,
					c.pkg, c.spec, got, ok, c.want, c.ok)
			}
		}
	})
	if n := strings.Count(logged, "neither linked nor declared"); n != 2 {
		t.Errorf("%d lines name an unresolved member, want the two cases no "+
			"importer above has and no self-reference:\n%s", n, logged)
	}
}

// A member's name an importer takes from the registry is that importer's hub
// label, nearest first: web's own @acme/lib over the root's link; no line.
func TestEdgeLabel_MemberNameFromTheRegistry(t *testing.T) {
	root, l := npmRepo(t)
	writeFile(t, filepath.Join(root, "workers/download/package.json"),
		`{"name": "download", "dependencies": {"api-gateway": "1.0.0"}}`)
	cases := []struct{ from, pkg, spec, to, want string }{
		{"workers/download/test/x.test.ts", "workers/download/test", "api-gateway", storeApiGateway,
			"@npm//workers/download:api-gateway"},
		{"web/src/a.ts", "web/src", "@acme/lib/wire", storeAcmeLib, "@npm//web:acme_lib"},
		{"web/vitest.config.mts", "packages/app/test", "@acme/lib/wire", storeAcmeLib, "@npm//web:acme_lib"},
	}
	var runtime []string
	out := captureLog(t, func() {
		for _, c := range cases {
			e := importEdge(c.from, c.spec, c.to)
			if got := l.edgeLabel(e, c.pkg, fixtureManifest(root, parentDir(e.From))); got != c.want {
				t.Errorf("%s imports %q: %q, want %q", c.from, c.spec, got,
					c.want)
			}
		}
		m := fixtureManifest(root, "workers/download/test")
		runtime = l.manifestLabels(m, "workers/download/test")
	})
	want := []string{"@npm//workers/download:api-gateway"}
	if !slices.Equal(runtime, want) {
		t.Errorf("manifestLabels = %v, want %v", runtime, want)
	}
	if out != "" {
		t.Errorf("log %q, want none", out)
	}
}

// A member importing a package it installed goes to the hub as anyone does.
func TestEdgeLabel_MemberInstalledPackage(t *testing.T) {
	root, l := npmRepo(t)
	e := importEdge("packages/lib/src/index.ts", "zod", storeZod)
	got := l.edgeLabel(e, "packages/lib", fixtureManifest(root, parentDir(e.From)))
	if got != "@npm//packages/lib:zod" {
		t.Errorf("edgeLabel = %q, want @npm//packages/lib:zod", got)
	}
	_, ok := l.memberView("zod", "packages/lib/src/index.ts", "packages/lib", fixtureManifest(root, "packages/lib/src"))
	if ok {
		t.Error("zod is a member")
	}
}

func TestLoadNpmLock_NoLockfile(t *testing.T) {
	if l, err := loadNpmLock(t.TempDir(), nil); err == nil {
		t.Errorf("loadNpmLock = %v, want an error", l)
	}
}
