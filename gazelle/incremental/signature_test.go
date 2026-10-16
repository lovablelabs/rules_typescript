package incremental

import (
	"cmp"
	"reflect"
	"testing"
)

// An edit that keeps a signature skips Gazelle, so every way a file can change
// its listing must move the signature, and the common edits must not.
func TestSignatureMovesExactlyWithTheListing(t *testing.T) {
	for _, tc := range []struct {
		name, file, before, after string
		same                      bool
	}{
		{"body edit", "a.ts", "import { x } from './x';\nexport const y = x + 1;\n", "import { x } from './x';\nexport const y = x + 2;\n", true},
		{"string edit", "a.tsx", "export const A = () => <p title=\"a\">Don't</p>;\n", "export const A = () => <p title=\"b\">It's</p>;\n", true},
		{"regex with quotes", "a.ts", "const r = /['\"]/g;\nexport {};\n", "const r = /['\"]+/g;\nexport {};\n", true},
		{"nested template", "a.ts", "export const s = `a${`b${1}`}`;\n", "export const s = `a${`c${2}`}`;\n", true},
		{"tag type arguments", "a.tsx", "export const A = () => <List<Item> items={[]} />;\n", "export const A = () => <List<Item> items={[1]} />;\n", true},
		{"generic arrow", "a.tsx", "export const f = <\n  T extends object,\n>(x: T) => x;\n", "export const f = <\n  T extends object,\n>(y: T) => y;\n", true},
		{"new import", "a.ts", "import './x';\n", "import './x';\nimport './y';\n", false},
		{"changed specifier", "a.ts", "export * from './x';\n", "export * from './y';\n", false},
		{"dynamic import inside JSX", "a.tsx", "export const A = () => <div>{import('./x')}</div>;\n", "export const A = () => <div>{import('./y')}</div>;\n", false},
		{"require in JavaScript", "a.js", "const x = require('x');\n", "const x = require('y');\n", false},
		{"JSDoc import type", "a.js", "/** @type {import('x').T} */\nexport let v;\n", "/** @type {import('y').T} */\nexport let v;\n", false},
		{"first JSX", "a.tsx", "export const a = 1;\n", "export const a = <b />;\n", false},
		{"reference directive", "a.ts", "/// <reference types=\"node\" />\nexport {};\n", "/// <reference types=\"bun\" />\nexport {};\n", false},
		{"augmentation", "a.ts", "declare module 'x' {}\nexport {};\n", "declare module 'y' {}\nexport {};\n", false},
		{"becomes a module", "a.ts", "declare module 'x' {}\n", "declare module 'x' {}\nexport {};\n", false},
		{"JSX pragma", "a.tsx", "/** @jsxImportSource react */\nexport const a = <b />;\n", "/** @jsxImportSource preact */\nexport const a = <b />;\n", false},
	} {
		before, err := Signature(tc.file, []byte(tc.before))
		if err != nil {
			t.Errorf("%s: before: %v", tc.name, err)
			continue
		}
		after, err := Signature(tc.file, []byte(tc.after))
		if err != nil {
			t.Errorf("%s: after: %v", tc.name, err)
			continue
		}
		if (before == after) != tc.same {
			t.Errorf("%s: same signature = %t, want %t", tc.name, before == after, tc.same)
		}
	}
}

func TestSignatureRefusesWhatItCannotRead(t *testing.T) {
	for _, src := range []string{"const s = 'unterminated\n", "export const a = () => <div>{x</div>;\n", "/* open"} {
		if _, err := Signature("a.tsx", []byte(src)); err == nil {
			t.Errorf("scanned %q", src)
		}
	}
}

// Where tsgo reads a source's imports otherwise than its scan, an edit between
// the two must move the scan or leave it untrusted, so no replay stands on it.
func TestScanMovesOrRefusesWhereTsgoReadsImportsOtherwise(t *testing.T) {
	for _, tc := range []struct{ name, before, after, file string }{
		{"a division after an object literal", "import { b } from './b';\nexport const c = b;\n", "import { b } from './b';\nconst n = {} / 2; import { d } from './d'; // '\nexport const c = b + n;\n", ""},
		{"a regex after an if head", "declare const s: string;\nif (s) /'/.test(s); // '\n", "declare const s: string;\nif (s) /'/.test(s); import \"./d\"; // '\n", ""},
		{"a regex after a for await head", "export {};\nfor await (const x of y) /'/.test(x); // '\n", "export {};\nfor await (const x of y) /'/.test(x); import \"./d\"; // '\n", ""},
		{"a path reference moved to the header", "export const c = 1;\n/// <reference path=\"./d.ts\" />\n", "/// <reference path=\"./d.ts\" />\nexport const c = 1;\n", ""},
		{"a pragma moved to the header", "export const v = 1;\n/** @jsxImportSource dep */\n", "/** @jsxImportSource dep */\nexport const v = 1;\n", ""},
		{"a division after a method named with", "import { b } from './b';\nexport const r = arr.with(0, 1) / 2; const q = b / 3;\n", "import { b } from './b';\nexport const r = arr.with(0, 1) / 2; import d from 'dep'; const q = b / 3;\n", ""},
		{"a division after a method named for", "import { b } from './b';\nexport const c = b;\n", "import { b } from './b';\nexport const c = b;\nconst q = a.for(1) / 2; import 'dep'; const r = 4 / 5;\n", ""},
		{"a division after a postfix increment", "let a = 1;\nconst z = a++ / 2; const w = 4 / 2;\nexport {};\n", "let a = 1;\nconst z = a++ / 2; import \"dep\"; const w = 4 / 2;\nexport {};\n", ""},
		{"a division after a property named default", "const z = o.default / 2; const w = 4 / 2;\nexport {};\n", "const z = o.default / 2; import \"dep\"; const w = 4 / 2;\nexport {};\n", ""},
		{"a pragma moved out of a JSX expression to the header", "export const v = <div>{/** @jsxImportSource dep */}</div>;\n", "/** @jsxImportSource dep */\nexport const v = <div>{}</div>;\n", "a.tsx"},
		{"a directive moved out of a JSX expression to the header", "export const v = <div>{\n/// <reference path=\"./d.ts\" />\n}</div>;\n", "/// <reference path=\"./d.ts\" />\nexport const v = <div>{\n}</div>;\n", "a.tsx"},
		{"a pragma on a later line of a pragma comment", "/**\n * @jsxRuntime automatic\n * @jsxImportSource react\n */\nexport {};\n", "/**\n * @jsxRuntime automatic\n * @jsxImportSource dep\n */\nexport {};\n", ""},
		{"an upper-case pragma", "/** @JSXImportSource react */\nexport {};\n", "/** @JSXImportSource dep */\nexport {};\n", ""},
		{"two pragmas swapped", "/** @jsxImportSource react */\n/** @jsxImportSource dep */\nexport {};\n", "/** @jsxImportSource dep */\n/** @jsxImportSource react */\nexport {};\n", ""},
		{"a dynamic import left after the last export", "declare module './b' {}\nexport const l = () => import('./b');\n", "declare module './b' {}\nconst l = () => import('./b');\n", ""},
		{"an import.meta inside a function removed", "declare module './b' {}\nfunction f() { return import.meta.url; }\n", "declare module './b' {}\nfunction f() { return 1; }\n", ""},
		{"an import moved out from under a do", "declare const x: boolean;\ndo import './d'; while (x);\nexport {};\n", "declare const x: boolean;\nimport './d';\ndo ; while (x);\nexport {};\n", ""},
		{"a type-only namespace alias left after the last export", "declare module './b' {}\nnamespace N { export type Y = 1; }\nimport type X = N.Y;\nexport {};\n", "declare module './b' {}\nnamespace N { export type Y = 1; }\nimport type X = N.Y;\n", ""},
		{"a string that reads like an import", "export const s = 'x';\n", "export const s = \"import('./d')\";\n", ""},
		{"a namespace alias left after the last export", "namespace N { export const y = 1; }\nimport x = N.y;\nexport {};\n", "namespace N { export const y = 1; }\nimport x = N.y;\n", ""},
		{"a property named export left after the last export", "declare const o: { export: number };\no.export = 1;\nexport {};\n", "declare const o: { export: number };\no.export = 1;\n", ""},
		{"a division after a non-null assertion", "declare const x: number;\nconst z = x! / 2; const w = 4 / 2;\nexport {};\n", "declare const x: number;\nconst z = x! / 2; import \"dep\"; const w = 4 / 2;\nexport {};\n", ""},
		{"a resolution-mode attribute", "import type { D } from './d';\n", "import type { D } from './d' with { 'resolution-mode': 'require' };\n", ""},
		{"an import beside a resolution-mode attribute made type-only", "import { D } from './d' with { 'resolution-mode': 'require' };\n", "import type { D } from './d' with { 'resolution-mode': 'require' };\n", ""},
		{"a resolution-mode attribute split by a line continuation changed", "import type { D } from './d' with { 'resolution\\\n-mode': 'import' };\n", "import type { D } from './d' with { 'resolution\\\n-mode': 'require' };\n", ""},
		{"an escaped resolution-mode attribute changed", "import type { D } from './d' with { 'resolution\\u002dmode': 'import' };\n", "import type { D } from './d' with { 'resolution\\u002dmode': 'require' };\n", ""},
	} {
		file := cmp.Or(tc.file, "a.ts")
		before, err := Scan(file, []byte(tc.before))
		if err != nil {
			t.Errorf("%s: before: %v", tc.name, err)
			continue
		}
		after, err := Scan(file, []byte(tc.after))
		if err != nil {
			t.Errorf("%s: after: %v", tc.name, err)
			continue
		}
		if trusted(before) && trusted(after) && reflect.DeepEqual(before, after) {
			t.Errorf("%s: both scans are trusted and equal", tc.name)
		}
	}
}

// trusted is a scan a replay may stand on.
func trusted(s Scanned) bool { return s.Counted && s.Exact }

// A JSON module's type attribute leaves its resolution as it was, so the scan stays trusted.
func TestScanCountsATypeAttribute(t *testing.T) {
	src, err := Scan("a.ts", []byte("import j from './j.json' with { type: 'json' };\nexport const k = j;\n"))
	if err != nil || !trusted(src) {
		t.Errorf("a type attribute left the scan untrusted: %v", err)
	}
}

// Prose and specifiers that only name "export" or "from" are no misread code, so the scan stays trusted.
func TestScanCountsTextThatOnlyNamesAnExport(t *testing.T) {
	for _, src := range []string{
		"// re-export for tests\nexport const a = 1;\n",
		"export const label = 'export';\n",
		"export const conditions = [\"import\", 'from'];\n",
		"import { csv } from './export';\nexport const c = csv;\n",
		"import f from './from';\nexport const g = () => import('../export/csv');\n",
	} {
		if scanned, err := Scan("a.ts", []byte(src)); err != nil || !trusted(scanned) {
			t.Errorf("%q left the scan untrusted: %v", src, err)
		}
	}
}

// Where tsgo reads a regular expression or a division, the scan reads the same and so
// sees the import beside it, trusted: no misread leaves it to the inexact fallback.
func TestScanReadsTheImportBesideADivisionOrRegexAsTsgoDoes(t *testing.T) {
	for _, src := range []string{
		"declare const s: string;\nif (s) /'/.test(s); import \"./d\"; // '\n",
		"export {};\nfor await (const x of y) /'/.test(x); import \"./d\"; // '\n",
		"let a = 1;\nconst z = a++ / 2; import \"./d\"; const w = 4 / 2;\nexport {};\n",
		"const z = o.default / 2; import \"./d\"; const w = 4 / 2;\nexport {};\n",
	} {
		scanned, err := Scan("a.ts", []byte(src))
		if err != nil || !trusted(scanned) || !reflect.DeepEqual(scanned.Specs, []string{"import:./d"}) {
			t.Errorf("%q: scanned %v (trusted %t), want import:./d, trusted: %v", src, scanned.Specs, trusted(scanned), err)
		}
	}
}
