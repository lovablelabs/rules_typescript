package incremental

import "testing"

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
