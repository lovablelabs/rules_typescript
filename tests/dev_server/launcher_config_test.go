package dev_server_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

// The generated vite.config.mjs reads every path it needs out of the environment,
// and //tools/launcher is what sets those variables from this JSON. So a test
// that evaluates the config has to hand it the same environment, or it is
// evaluating a different config than `bazel run` does.
type launcherConfig struct {
	DevServer struct {
		ConfigFile  string `json:"config_file"`
		NodeModules string `json:"node_modules"`
		Plugin      string `json:"plugin"`
		UserConfig  string `json:"user_config"`
	} `json:"dev_server"`
}

func readLauncherConfig(t *testing.T, tree *verify.Tree, target string) launcherConfig {
	t.Helper()
	var cfg launcherConfig
	tree.File("tests/dev_server/" + target + "_launcher.json").JSON(&cfg)
	return cfg
}

// env is the launcher's contribution to the config, as environment variables.
func (c launcherConfig) env(tree *verify.Tree, workspace string) []string {
	env := []string{"BUILD_WORKSPACE_DIRECTORY=" + workspace}
	for name, rlocation := range map[string]string{
		"NODE_MODULES_PATH":     c.DevServer.NodeModules,
		"VITE_PLUGIN_PATH":      c.DevServer.Plugin,
		"VITE_USER_CONFIG_PATH": c.DevServer.UserConfig,
	} {
		if rlocation == "" {
			continue
		}
		env = append(env, name+"="+inTree(tree, rlocation))
	}
	return env
}

func inTree(tree *verify.Tree, rlocation string) string {
	return filepath.Join(filepath.Dir(tree.Path(".")), filepath.FromSlash(rlocation))
}

func runfilesDir(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
		if dir == "" {
			continue
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	t.Fatal("no runfiles tree: RUNFILES_DIR and TEST_SRCDIR are both unusable")
	return ""
}
