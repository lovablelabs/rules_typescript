// The vite_config boundary: where the rule loads a user-supplied config from,
// and therefore what that config is allowed to import.
//
// Node resolves a runfiles symlink to the source file before it resolves that
// file's own imports, so a vite_config loaded out of the source tree resolves
// its imports against a source-tree node_modules -- which this ruleset does not
// have, and which no part of the build graph would know about if it did. So the
// rule loads a COPY in bin, beside the npm tree the node_modules attr built, and
// that copy is what draws the boundary:
//
//   - the launcher is pointed at the copy, not at the source file;
//   - a BARE npm specifier in the config resolves through that tree
//     (:dev_with_user_config_behaviour_test proves it over HTTP, since only a
//     running server shows the plugin actually installed);
//   - a RELATIVE import resolves only when the module it names is declared in
//     vite_config_srcs, because that is what stages it beside the copy. An
//     UNDECLARED sibling is not there, and the dev server refuses to start,
//     naming the file, rather than coming up with half a config.
//
// Also here, because it is a property of the same generated file: the config
// reaches @vitejs/plugin-react through that package's own exports map, never
// through a path into its dist/. That the resolution works is proved by the
// react behaviour tests; what is asserted here is that it is resolution at all.
package dev_server_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikn/rules_typescript/tests/verify"
)

func TestUserConfigIsLoadedFromBin(t *testing.T) {
	tree := verify.New(t)
	cfg := readLauncherConfig(t, tree, "dev_with_user_config")

	got := cfg.DevServer.UserConfig
	if got == "" {
		t.Fatal("the launcher config carries no user_config path")
	}
	if strings.HasSuffix(got, "/tests/dev_server/user_config.mjs") {
		t.Errorf("the launcher loads the vite_config source file (%s), whose own imports "+
			"then resolve in the source tree", got)
	}
	if !strings.Contains(got, "dev_with_user_config_dev/") {
		t.Errorf("user_config is %q, want the generated copy under dev_with_user_config_dev/", got)
	}
	if !tree.File(strings.SplitN(got, "/", 2)[1]).Exists() {
		t.Errorf("the launcher's user_config path %q is not in runfiles", got)
	}
}

func TestRelativeImportFromUserConfigFails(t *testing.T) {
	tree := verify.New(t)
	launcher := tree.File("tests/dev_server/dev_with_relative_user_config_launcher")
	helper := tree.File("tests/dev_server/user_config_helper.mjs")
	if !launcher.Exists() || !helper.Exists() {
		t.FailNow()
	}

	// The sibling module exists, right where the config imports it from. What it
	// does not exist beside is the copy, and the copy is what gets loaded.
	// A deadline well inside the test's own, because the failure mode this rules
	// out is a server that comes up and serves: that one runs until something
	// kills it, and this way the report is the assertion rather than a panic.
	const deadline = 20 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "tests", "dev_server"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, launcher.Abs())
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"RUNFILES_DIR="+runfilesDir(t),
		"BUILD_WORKSPACE_DIRECTORY="+ws,
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the dev server was still running after %s, so a relative import out "+
			"of a vite_config resolved to something:\n%s", deadline, out)
	}
	if err == nil {
		t.Fatalf("the dev server exited cleanly, so it loaded the vite_config:\n%s", out)
	}
	report := string(out)
	for _, want := range []string{
		"[rules_typescript] Failed to load vite_config",
		"user_config_helper.mjs",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the failure does not mention %q, so it does not tell the user what "+
				"happened:\n%s", want, report)
		}
	}
}

func TestReactPluginEntryIsNotHardcoded(t *testing.T) {
	tree := verify.New(t)
	config := tree.File("tests/dev_server/dev_with_react_refresh_dev/vite.config.mjs")
	if !config.Exists() {
		t.FailNow()
	}

	// A path into another package's build output is this rule guessing at a layout
	// that package owns -- and reorganises: the entry point moved between the two
	// majors this repo has built against.
	config.ExcludesRE(`@vitejs/plugin-react/[A-Za-z0-9_.-]+`)
	config.Contains("npmEntryPath('@vitejs/plugin-react')", "manifest.exports")
}

// oj defers a Cloudflare app's client bundle only when its --config text names the plugin.
func TestCloudflareConfigNamesThePluginForOj(t *testing.T) {
	verify.New(t).File("tests/dev_server/dev_cloudflare_dev/vite.config.mjs").Contains("@cloudflare/vite-plugin")
}

// oj resolves its plugins' config at logLevel 'silent'; the Cloudflare plugin logs the Worker's errors to its logger.
func TestCloudflareConfigLogsWorkerErrors(t *testing.T) {
	tree := verify.New(t)
	node := tree.File("ts/toolchain/node_resolved/node")
	config := tree.File("tests/dev_server/dev_cloudflare_dev/vite.config.mjs")
	if !node.Exists() || !config.Exists() {
		t.FailNow()
	}
	cmd := exec.Command(node.Abs(), "--input-type=module", "-e",
		"const { default: config } = await import(process.argv[1]); config.customLogger.error('worker failed');",
		config.Abs())
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "bazel-bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), readLauncherConfig(t, tree, "dev_cloudflare").env(tree, ws)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("evaluating the config: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "worker failed") {
		t.Errorf("customLogger.error wrote nothing to stderr:\n%s", stderr.String())
	}
}
