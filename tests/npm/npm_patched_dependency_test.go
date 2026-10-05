package npm_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// The registry tarball for nanoid@3.3.11 is byte-identical whether or not the
// patch is applied -- `packages:` records the integrity of the unpatched publish
// -- so nothing but the file content can tell the two apart. The fixture patch
// rewrites "sideEffects": false into an array -- a realistic patch, since a
// wrong sideEffects lets a bundler tree-shake away a module with side effects.
func TestPatchReachesTheFilesBazelHandsOut(t *testing.T) {
	tree := verify.New(t)

	var pkg struct {
		SideEffects json.RawMessage `json:"sideEffects"`
	}
	f := tree.FoundFile("*/.pnpm/nanoid@3.3.11/node_modules/nanoid/package.json")
	f.JSON(&pkg)

	want := []string{"./index.js", "./index.cjs"}
	var got []string
	if err := json.Unmarshal(pkg.SideEffects, &got); err != nil || !slices.Equal(got, want) {
		t.Errorf("%s was not patched: sideEffects is %s, want %q\n"+
			"  The published nanoid@3.3.11 has \"sideEffects\": false; the patch in\n"+
			"  tests/npm/patches/nanoid@3.3.11.patch replaces it with the array.",
			f.Name(), pkg.SideEffects, want)
	}
}

// The same mechanism for a SCOPED package, which is where the filename pnpm
// derives from the package name starts with '@' -- `@types__ms@2.1.0.patch` --
// and where the patch lands in a .d.ts rather than in the manifest. The
// published @types/ms@2.1.0 types the options object as `{ long: boolean }`,
// which makes the field mandatory as soon as the object is passed at all.
func TestScopedPackagePatchReachesTheFilesBazelHandsOut(t *testing.T) {
	tree := verify.New(t)

	f := tree.FoundFile("*/.pnpm/@types+ms@2.1.0/node_modules/@types/ms/index.d.ts")
	f.Contains("options?: { long?: boolean }")
	f.Excludes("options?: { long: boolean }")
}
