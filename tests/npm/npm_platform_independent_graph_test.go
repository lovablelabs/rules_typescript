package npm_test

import (
	"slices"
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// fsevents@2.3.3 declares `os: [darwin]`. Whether it belongs in a particular
// build is a property of the TARGET platform, so it is a select(); whether it
// exists at all must not be a property of the machine that evaluated the module
// extension, because that machine's answer is what lands in MODULE.bazel.lock.
func TestForeignPlatformPackageIsInTheGraph(t *testing.T) {
	tree := verify.New(t)

	var pkg struct {
		OS []string `json:"os"`
	}
	f := tree.FoundFile("*/.pnpm/fsevents@2.3.3/node_modules/fsevents/package.json")
	f.JSON(&pkg)

	if !slices.Equal(pkg.OS, []string{"darwin"}) {
		t.Errorf("%s has os=%q, want [darwin]\n"+
			"  The fixture no longer exercises a foreign-platform package; pick another.",
			f.Name(), pkg.OS)
	}
}
