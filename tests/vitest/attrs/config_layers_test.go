package attrs_test

import (
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// The provider has no observable effect from inside a passing test, so the
// generated config itself is what gets pinned here, with the layer order.
func TestTheGeneratedConfigLayersBazelUserProviderAndSnapshots(t *testing.T) {
	tree := verify.New(t)

	config := tree.File("tests/vitest/attrs/_attrs_test.vitest/config.mjs")
	config.Contains(
		`from './vitest.config.mts';`,
		`const ROOT = resolve(HERE, ".");`,
		`name: 'rules_typescript:module-ids',`,
		`server: { fs: { allow: FS_ALLOW } },`,
		`const providerLayer = { test: { coverage: { provider: "v8" } } };`,
		`merge(merge(merge(bazelLayer, user), providerLayer), snapshotLayer)`,
		`const merged = withCompiledRun(merge(`,
		`withCompiledRun(merge(bazelLayer, p), merged.root, user.root == null) : p,`,
	)
	config.Excludes(
		"attrLayer", "environment:", "setupFiles: [abs(", "globals: true",
	)
}

func TestTheGeneratedConfigDoesNotListIndividualTestFiles(t *testing.T) {
	tree := verify.New(t)

	config := tree.File("tests/vitest/attrs/_attrs_test.vitest/config.mjs")
	config.Contains(
		`const INCLUDE = ["**/*.{test,spec}.ts"];`,
		`const BIN_PROBE = "tests/vitest/attrs/attrs.test.js";`,
		`INCLUDE.map((pattern) => '../'.repeat(level) + pattern)).flat(),`,
	)
	config.Excludes(
		`"attrs.test.js"]`, "for (const f of INCLUDE)",
	)
}

// vitest walks the root the launcher staged, not the runfiles tree: `dir` is
// set beside `include`, after the merge, on the root and on every project.
func TestTheGeneratedConfigWalksTheStagedRoot(t *testing.T) {
	tree := verify.New(t)

	config := tree.File("tests/vitest/attrs/_attrs_test.vitest/config.mjs")
	config.Contains(
		`const FILES_ROOT = process.env.TS_TEST_FILES_ROOT;`,
		`const dir = resolve(DISCOVERY_ROOT, relative(RUNFILES_ROOT, root));`,
	)
}
