package npm_test

import (
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

func TestFileTarballDependencyIsExtractedFromTheWorkspace(t *testing.T) {
	tree := verify.New(t)

	tree.FoundFile("*/node_modules/@vendored/local-pkg/index.js").Contains("from a local tarball")
	tree.FoundFile("*/node_modules/@vendored/local-pkg/index.d.ts").Contains("vendored: string")
}
