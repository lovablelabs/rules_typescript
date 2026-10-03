package npm_test

import (
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// @types/express-serve-static-core@4.19.6 packs its files under
// "express-serve-static-core v4.19/", so the reader must take the prefix out of
// the archive; predicting "express-serve-static-core" fails the fetch.
func TestUnpredictableTarballPrefixIsStripped(t *testing.T) {
	tree := verify.New(t)

	dir := tree.FoundDir("*/.pnpm/@types+express-serve-static-core@4.19.6/node_modules/@types/express-serve-static-core")
	dir.File("index.d.ts").Exists()
	dir.File("package.json").Exists()

	dir.Absent("express-serve-static-core v4.19")
}
