package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// serverCommand builds the argv for the selected dev server: a path under the
// importer's node_modules, or a native binary from the runfiles.
func serverCommand(cfg *Config, r *Resolver, configFile, nodeModules, root string, port int) ([]string, string, error) {
	d := cfg.DevServer
	var argv []string
	var serverPath string

	if d.ServerInTree != "" {
		if nodeModules == "" {
			return nil, "", fmt.Errorf(
				"ts_dev_server: %s selected a server that ships as an npm package (%s), "+
					"but the target has no node_modules attr.\n"+
					"Add node_modules = \":node_modules\" pointing at a node_modules() target "+
					"whose deps include that package; there is no host-PATH fallback.",
				cfg.Label, d.ServerInTree)
		}
		serverPath = filepath.Join(nodeModules, filepath.FromSlash(d.ServerInTree))
		if !fileExists(serverPath) {
			return nil, "", fmt.Errorf(
				"ts_dev_server: the dev server is missing from the node_modules of %s:\n"+
					"                 %s\n"+
					"                 Add the package providing it to the deps of that "+
					"node_modules() target.",
				cfg.Label, serverPath)
		}
	} else {
		p, err := r.Path(d.ServerBinary)
		if err != nil {
			return nil, "", fmt.Errorf(
				"ts_dev_server: the dev server binary is missing from the runfiles of %s: %w\n"+
					"Re-run via `bazel run %s`.", cfg.Label, err, cfg.Label)
		}
		serverPath = p
	}

	if d.RunsInJsRuntime {
		runtime, err := runtimeCommand(cfg, r)
		if err != nil {
			return nil, "", fmt.Errorf(
				"ts_dev_server: the toolchain JS runtime is missing from the runfiles of %s: %w\n"+
					"Re-run via `bazel run %s`; a host node is not used.", cfg.Label, err, cfg.Label)
		}
		argv = append(runtime, serverPath)
	} else {
		argv = []string{serverPath}
	}
	named := false
	for _, a := range d.Argv {
		if strings.Contains(a, "{port}") {
			named = true
		}
		a = strings.ReplaceAll(a, "{config}", configFile)
		a = strings.ReplaceAll(a, "{root}", root)
		a = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
		argv = append(argv, a)
	}
	// A server whose argv does not name the port reads it from the config, and
	// the flag is how an override reaches it -- Vite's CLI --port beats what the
	// config says. One whose argv does name it has already been given the same
	// number, and passing it twice is an error rather than a later-wins.
	if !named {
		argv = append(argv, "--port", strconv.Itoa(port))
	}
	return argv, serverPath, nil
}

func planDevServer(cfg *Config, r *Resolver, plan *Plan, args []string) (*Plan, error) {
	d := cfg.DevServer
	if cfg.Runtime == "" {
		return nil, fmt.Errorf(
			"ts_dev_server: %s resolved no JS runtime toolchain.\n"+
				"Did you mean to register the toolchains in MODULE.bazel?\n"+
				"    register_toolchains(\"@rules_typescript//ts/toolchain:all\")", cfg.Label)
	}
	configFile, err := r.Path(d.ConfigFile)
	if err != nil {
		return nil, err
	}
	if d.NodeModules == "" {
		return nil, fmt.Errorf(
			"ts_dev_server: %s has no node_modules attr, so the app's own dependencies "+
				"are not in runfiles.\n"+
				"Add node_modules = \":node_modules\" pointing at a node_modules() target; "+
				"the generated config resolves every bare specifier through its links.",
			cfg.Label)
	}
	nodeModules, err := r.Path(d.NodeModules)
	if err != nil {
		return nil, err
	}
	plan.setEnv("NODE_MODULES_PATH", nodeModules)

	// NODE_PATH as well, which Node's own ESM resolution ignores -- but not every
	// resolver in the graph is Node's. Tailwind v4 resolves `@import "tailwindcss"`
	// from the CSS file's own directory using enhanced-resolve, which reads
	// NODE_PATH explicitly; a source-tree .css has no node_modules above it to
	// walk up to, so without this the dev server answers 500 for that stylesheet.
	plan.prependPath("NODE_PATH", nodeModules)

	workspace := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspace == "" {
		workspace, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	bazelBin := filepath.Join(workspace, "bazel-bin")
	plan.setEnv("BAZEL_BIN_DIR", bazelBin)

	appRoot := filepath.Join(workspace, filepath.FromSlash(d.PackageDir))
	plan.Dir = appRoot
	plan.setEnv("BUILD_WORKSPACE_DIRECTORY", workspace)

	// A server whose argv names the port takes the override there; one that reads
	// it from the config still gets it appended, which is where it looked before.
	port, args := portOverride(d.Port, args)
	argv, serverPath, err := serverCommand(cfg, r, configFile, nodeModules, appRoot, port)
	if err != nil {
		return nil, err
	}

	// A native server still runs a Node plugin host, so the toolchain node has
	// to be findable by name rather than only by the argv above.
	if !d.RunsInJsRuntime {
		if runtime, err := runtimeCommand(cfg, r); err == nil && len(runtime) > 0 {
			plan.setEnv("PATH", filepath.Dir(runtime[0])+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}

	for name, rl := range map[string]string{
		"VITE_PLUGIN_PATH":      d.Plugin,
		"VITE_USER_CONFIG_PATH": d.UserConfig,
	} {
		if rl == "" {
			continue
		}
		p, err := r.Path(rl)
		if err != nil {
			return nil, err
		}
		plan.setEnv(name, p)
	}
	// Server caches belong in the output tree, outside the sources being watched.
	scratch := filepath.Join(bazelBin, filepath.FromSlash(d.ScratchDir))
	for name, dir := range map[string]string{
		"TSR_TMP_DIR":  filepath.Join(scratch, "tanstack-tmp"),
		"OJ_CACHE_DIR": filepath.Join(scratch, "oj-cache"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("ts_dev_server: cannot create %s at %s: %w", name, dir, err)
		}
		plan.setEnv(name, dir)
	}

	anchor, err := anchorNodeModules(workspace, nodeModules, plan)
	if err != nil {
		return nil, err
	}

	plan.Messages = append(plan.Messages,
		fmt.Sprintf("[ts_dev_server] Starting dev server on port %d...", port),
		fmt.Sprintf("[ts_dev_server] Workspace: %s", workspace),
		fmt.Sprintf("[ts_dev_server] bazel-bin: %s", bazelBin),
		fmt.Sprintf("[ts_dev_server] node_modules: %s", nodeModules),
		fmt.Sprintf("[ts_dev_server] Config: %s", configFile),
		fmt.Sprintf("[ts_dev_server] Server: %s", serverPath),
	)
	if anchor != "" {
		plan.Messages = append(plan.Messages, fmt.Sprintf(
			"[ts_dev_server] Linked %s -> the node_modules; removed on Ctrl-C. "+
				"Add `node_modules` (no trailing slash) to .gitignore.", anchor))
	}

	plan.Argv = append(argv, args...)
	plan.UseExec = false
	// ibazel SIGTERMs the runner after every rebuild; the server has to survive
	// that and pick the new .js up through its watcher, so only Ctrl-C stops it.
	plan.Supervise = SuperviseOptions{IgnoreTerm: true, ExitZeroOnInterrupt: true, TerminateOnInterrupt: true}
	return plan, nil
}

// anchorNodeModules links the importer's node_modules in as <workspace>/
// node_modules (docs/guides/dev-server.md § How a Bare npm Specifier Resolves).
func anchorNodeModules(workspace, nodeModules string, plan *Plan) (string, error) {
	link := filepath.Join(workspace, "node_modules")
	switch target, err := os.Readlink(link); {
	case err == nil && target == nodeModules:
		return "", nil
	case err == nil:
		return "", fmt.Errorf(
			"ts_dev_server: %s is already a symlink to a different node_modules:\n"+
				"                 have %s\n"+
				"                 want %s\n"+
				"A dev server resolves bare specifiers by walking up from the "+
				"workspace root, so the two cannot both be there. Stop the "+
				"other dev server, or point both targets at one node_modules().",
			link, target, nodeModules)
	}
	if _, err := os.Lstat(link); err == nil {
		return "", fmt.Errorf(
			"ts_dev_server: %s already exists and is not a symlink.\n"+
				"That is usually a `pnpm install` tree. The dev server resolves "+
				"through the Bazel node_modules at\n"+
				"                 %s\n"+
				"and will not delete an install to get there -- remove it yourself "+
				"if Bazel should own the dependencies.", link, nodeModules)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Symlink(nodeModules, link); err != nil {
		return "", fmt.Errorf(
			"ts_dev_server: could not link %s -> %s: %w\n"+
				"On Windows a symlink needs Developer Mode or an elevated shell.",
			link, nodeModules, err)
	}
	// Supervise.IgnoreTerm swallows ibazel's per-rebuild SIGTERM, so this runs
	// on Ctrl-C; the link only, never its target.
	if err := plan.own(link, false); err != nil {
		return "", err
	}
	return link, nil
}

// Some servers reject repeated --port flags instead of accepting the last value.
func portOverride(port int, args []string) (int, []string) {
	kept := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		flag, value := args[i], ""
		if eq := strings.IndexByte(flag, '='); eq >= 0 {
			flag, value = flag[:eq], flag[eq+1:]
		} else if flag == "-p" || flag == "--port" {
			if i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		if flag != "-p" && flag != "--port" {
			kept = append(kept, args[i])
			continue
		}
		if parsed, err := strconv.Atoi(value); err == nil {
			port = parsed
		}
	}
	return port, kept
}
