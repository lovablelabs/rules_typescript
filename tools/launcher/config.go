// Command ts_launcher runs the Node.js process described by a JSON config that
// a rules_typescript rule wrote beside it at analysis time. One checked-in
// binary replaces the shell script each rule used to generate.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/runtimeview"
)

// ConfigEnvVar overrides where the config is read from.
const ConfigEnvVar = "TS_LAUNCHER_CONFIG"

// DumpEnvVar makes the launcher print the resolved config and exit.
const DumpEnvVar = "TS_LAUNCHER_DUMP_CONFIG"

// DumpFlag is the first-argument form of DumpEnvVar.
const DumpFlag = "--dump-config"

const (
	ModeNode      = "node"
	ModeVitest    = "vitest"
	ModeNodeTest  = "node_test"
	ModeDevServer = "devserver"
)

// Config is the whole contract between the Starlark rules and this binary.
type Config struct {
	Label            string            `json:"label"`
	Mode             string            `json:"mode"`
	Workspace        string            `json:"workspace"`
	Runtime          string            `json:"runtime,omitempty"`
	RunArgs          []string          `json:"runtime_args,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	RuntimeModules   []string          `json:"runtime_modules,omitempty"`
	NativeViewAnchor string            `json:"native_view_anchor,omitempty"`

	Node      *NodeConfig      `json:"node,omitempty"`
	Vitest    *VitestConfig    `json:"vitest,omitempty"`
	NodeTest  *NodeTestConfig  `json:"node_test,omitempty"`
	DevServer *DevServerConfig `json:"dev_server,omitempty"`
}

// NodeConfig runs one .js entry point. NodeModules, here and below, is an
// importer's node_modules: a directory no manifest entry names.
type NodeConfig struct {
	Entry        string        `json:"entry"`
	NodeModules  string        `json:"node_modules,omitempty"`
	OptionalDeps []PackageLink `json:"optional_deps,omitempty"`
}

type NpmContext = runtimeview.NpmContext

type NpmPackageScope = runtimeview.NpmPackageScope

type PackageLink = runtimeview.PackageLink

// VitestConfig runs the vitest CLI over a sharded set of compiled test files.
// NodeModules is the importer chain, nearest first.
type VitestConfig struct {
	NpmContexts   []NpmContext        `json:"npm_contexts,omitempty"`
	NpmBindings   []map[string]string `json:"npm_binding_sets,omitempty"`
	VitestInTree  string              `json:"vitest_in_tree,omitempty"`
	ConfigFile    string              `json:"config_file"`
	TestFilesList string              `json:"test_files_list"`
	NodeModules   []string            `json:"node_modules,omitempty"`
	ReadsHook     string              `json:"reads_hook,omitempty"`
	// RootRel is vite's root relative to the config's directory: the package
	// of a `config` from an ancestor package, "." otherwise.
	RootRel string `json:"root_rel,omitempty"`
	// Stage maps a runfiles path to the runfiles path the launcher writes its
	// content to as a regular file; ConfigFile is one of the destinations.
	Stage map[string]string `json:"stage,omitempty"`
}

// NodeTestConfig carries no config file: node:test is configured by CLI flags
// and by the test file itself.
type NodeTestConfig struct {
	TestFilesList string   `json:"test_files_list"`
	NodeModules   []string `json:"node_modules,omitempty"`
	// ResolveHook is the ESM resolver shim that answers the `./x.ts` specifier
	// oxc emits verbatim into the .js beside it. Empty disables it.
	ResolveHook string `json:"resolve_hook,omitempty"`
}

// DevServerConfig runs one dev server implementation. Exactly one of
// ServerBinary (a runfile) and ServerInTree (a path under NodeModules) is set.
type DevServerConfig struct {
	PackageDir      string   `json:"package_dir,omitempty"`
	ConfigFile      string   `json:"config_file"`
	NodeModules     string   `json:"node_modules,omitempty"`
	ServerBinary    string   `json:"server_binary,omitempty"`
	ServerInTree    string   `json:"server_in_tree,omitempty"`
	Argv            []string `json:"argv"`
	RunsInJsRuntime bool     `json:"runs_in_js_runtime,omitempty"`
	Plugin          string   `json:"plugin,omitempty"`
	UserConfig      string   `json:"user_config,omitempty"`
	// ScratchDir is this target's directory under bazel-bin, where a dev server
	// that insists on writing somewhere is pointed instead of the source tree.
	ScratchDir string `json:"scratch_dir,omitempty"`
	Port       int    `json:"port"`
}

// LoadConfig reads the config for this launcher.
func LoadConfig(argv0 string, r *Resolver) (*Config, string, error) {
	path, err := configPath(argv0, r)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("ts_launcher: reading config %s: %w", path, err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, path, fmt.Errorf("ts_launcher: parsing config %s: %w", path, err)
	}
	return cfg, path, nil
}

// configPath finds the per-target config, staged at the runfiles root under
// "<launcher basename>.json" -- reachable from `bazel run`, `bazel test`, and
// from another rule's action, where argv[0] has nothing beside it.
func configPath(argv0 string, r *Resolver) (string, error) {
	if p := os.Getenv(ConfigEnvVar); p != "" {
		return p, nil
	}
	if argv0 == "" {
		return "", errors.New("ts_launcher: argv[0] is empty and " + ConfigEnvVar + " is unset")
	}
	name := filepath.Base(argv0) + ".json"
	tried := []string{}
	if r != nil {
		tried = append(tried, "runfiles:"+name)
		if p, err := r.Path(name); err == nil && isRegular(p) {
			return p, nil
		}
	}
	for _, base := range candidateArgv0(argv0) {
		candidate := base + ".json"
		tried = append(tried, candidate)
		if isRegular(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"ts_launcher: no config found for argv[0] %q (tried %s).\n"+
			"The rule stages <launcher>.json in the launcher's runfiles; set %s to point at it.",
		argv0, strings.Join(tried, ", "), ConfigEnvVar)
}

func isRegular(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func candidateArgv0(argv0 string) []string {
	out := []string{argv0}
	if !filepath.IsAbs(argv0) {
		if cwd, err := os.Getwd(); err == nil {
			out = append(out, filepath.Join(cwd, argv0))
		}
	}
	return out
}

// ParseConfig decodes and validates a config document.
func ParseConfig(data []byte) (*Config, error) {
	cfg := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.Mode {
	case ModeNode:
		if c.Node == nil {
			return errors.New(`mode "node" requires a "node" section`)
		}
		if c.Node.Entry == "" {
			return errors.New(`mode "node" requires node.entry`)
		}
	case ModeVitest:
		if c.Vitest == nil {
			return errors.New(`mode "vitest" requires a "vitest" section`)
		}
		if c.Vitest.ConfigFile == "" || c.Vitest.TestFilesList == "" {
			return errors.New(`mode "vitest" requires vitest.config_file and vitest.test_files_list`)
		}
	case ModeNodeTest:
		if c.NodeTest == nil {
			return errors.New(`mode "node_test" requires a "node_test" section`)
		}
		if c.NodeTest.TestFilesList == "" {
			return errors.New(`mode "node_test" requires node_test.test_files_list`)
		}
	case ModeDevServer:
		if c.DevServer == nil {
			return errors.New(`mode "devserver" requires a "dev_server" section`)
		}
		if c.DevServer.ConfigFile == "" {
			return errors.New(`mode "devserver" requires dev_server.config_file`)
		}
		if (c.DevServer.ServerBinary == "") == (c.DevServer.ServerInTree == "") {
			return errors.New(`mode "devserver" requires exactly one of dev_server.server_binary and dev_server.server_in_tree`)
		}
	case "":
		return errors.New(`missing "mode"`)
	default:
		return fmt.Errorf("unknown mode %q (want %q, %q, %q or %q)",
			c.Mode, ModeNode, ModeVitest, ModeNodeTest, ModeDevServer)
	}
	return nil
}
