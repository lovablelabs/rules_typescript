package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func nativeResolver(cfg *Config, original *Resolver, plan *Plan) (*Resolver, error) {
	if cfg.NativeViewAnchor == "" {
		return nil, fmt.Errorf("ts_launcher: native execution requires a build-owned runtime view; rebuild with the matching ruleset tools")
	}
	if !fs.ValidPath(cfg.NativeViewAnchor) || !filepath.IsLocal(filepath.FromSlash(cfg.NativeViewAnchor)) || !strings.HasSuffix(cfg.NativeViewAnchor, ".json") {
		return nil, fmt.Errorf("ts_launcher: invalid native view anchor %q", cfg.NativeViewAnchor)
	}
	config, err := original.Path(cfg.NativeViewAnchor)
	if err != nil {
		return nil, err
	}
	config, err = filepath.EvalSymlinks(config)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(strings.TrimSuffix(config, ".json")+".runtime", "view")
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ts_launcher: native view %q is not a directory", root)
	}
	r, err := directoryResolver(root)
	if err != nil {
		return nil, err
	}
	plan.runfilesEnv = r.Env()
	return r, nil
}

func nativeNodePath(r *Resolver, plan *Plan, paths []string) error {
	for i := len(paths) - 1; i >= 0; i-- {
		path, err := r.Path(paths[i])
		if err != nil {
			return err
		}
		if isDir(path) {
			plan.prependPath("NODE_PATH", path)
		}
	}
	return nil
}

func planNode(cfg *Config, original *Resolver, plan *Plan, args []string) (*Plan, error) {
	argv, err := runtimeCommand(cfg, original)
	if err != nil {
		return nil, err
	}
	r, err := nativeResolver(cfg, original, plan)
	if err != nil {
		return nil, err
	}
	entry, err := r.Path(cfg.Node.Entry)
	if err != nil {
		return nil, err
	}
	if cfg.Node.NodeModules != "" {
		if err := nativeNodePath(r, plan, []string{cfg.Node.NodeModules}); err != nil {
			return nil, err
		}
	}
	if len(cfg.Node.OptionalDeps) > 0 {
		plan.prependPath("NODE_PATH", filepath.Join(r.Dir(), "..", "optional", "node_modules"))
	}
	plan.Argv = append(append(argv, entry), args...)
	return plan, nil
}
