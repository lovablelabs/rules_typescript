package main

import (
	"fmt"
	"os"
)

func planNodeTest(
	cfg *Config, original *Resolver, plan *Plan, args []string, shard Shard,
) (_ *Plan, err error) {
	n := cfg.NodeTest

	// A coverage run asks for a report node --test cannot write, and an empty
	// one would read as a clean run.
	if os.Getenv("COVERAGE_DIR") != "" {
		return nil, fmt.Errorf(
			"ts_test: the node:test runner does not report coverage. " +
				"Run `bazel coverage` against vitest targets, or `bazel test` " +
				"against this one.")
	}

	argv, err := runtimeCommand(cfg, original)
	if err != nil {
		return nil, err
	}
	r, err := nativeResolver(cfg, original, plan)
	if err != nil {
		return nil, err
	}
	listed, err := testFiles(r, n.TestFilesList)
	if err != nil {
		return nil, err
	}
	files := shardFiles(listed, shard)
	if len(files) == 0 {
		return emptyShard(plan, shard), nil
	}

	plan.Dir = r.Dir()
	if err := nativeNodePath(r, plan, n.NodeModules); err != nil {
		return nil, err
	}
	if n.ResolveHook != "" {
		hook, err := r.Path(n.ResolveHook)
		if err != nil {
			return nil, err
		}
		// --import, not NODE_OPTIONS: node --test runs each file in a child
		// process and forwards the parent's execArgv to it.
		argv = append(argv, "--import", hook)
	}
	argv = append(argv, args...)
	argv = append(argv, "--test")

	// --test_filter reaches a test runner as TESTBRIDGE_TEST_ONLY; node:test
	// takes it as a regular expression over test names.
	if only := os.Getenv("TESTBRIDGE_TEST_ONLY"); only != "" {
		argv = append(argv, "--test-name-pattern", only)
	}

	for _, f := range files {
		path, err := r.Path(f.rlocation)
		if err != nil {
			return nil, err
		}
		argv = append(argv, path)
	}
	plan.Argv = argv
	return plan, nil
}
