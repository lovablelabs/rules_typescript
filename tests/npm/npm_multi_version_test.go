package npm_test

import (
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// The fixture lockfile has @rolldown/pluginutils at 1.0.0-rc.3 and 1.0.1, which
// is what exercises the multi-version label generation:
//
//	@npm//:rolldown_pluginutils_1_0_0_rc_3  versioned target for 1.0.0-rc.3
//	@npm//:rolldown_pluginutils_1_0_1       versioned target for 1.0.1
//	@npm//:rolldown_pluginutils             alias to the highest version
func TestVersionedLabelsStayApart(t *testing.T) {
	tree := verify.New(t)

	for _, c := range []struct{ storeKey, want string }{
		{"@rolldown+pluginutils@1.0.0-rc.3", "1.0.0-rc.3"},
		{"@rolldown+pluginutils@1.0.1", "1.0.1"},
	} {
		dir := tree.FoundDir("*/.pnpm/" + c.storeKey + "/node_modules/@rolldown/pluginutils")
		var pkg struct {
			Version string `json:"version"`
		}
		dir.File("package.json").JSON(&pkg)
		if pkg.Version != c.want {
			t.Errorf("%s/package.json is version %q, want %q", dir.Name(), pkg.Version, c.want)
		}
	}
}
