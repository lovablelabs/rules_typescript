package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// readsRun is one --reads run: the workspace the report is relative to
// and the file the hook appends every read outside the runfiles to.
type readsRun struct {
	workspace string
	record    string
}

func startReads(label string) (*readsRun, error) {
	ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if ws == "" {
		return nil, fmt.Errorf("ts_test: %s reads the workspace, which "+
			"only `bazel run` names: BUILD_WORKSPACE_DIRECTORY is unset.", label)
	}
	ws, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "ts_test_reads")
	if err != nil {
		return nil, err
	}
	f.Close()
	return &readsRun{workspace: ws, record: f.Name()}, nil
}

func (x *readsRun) install(plan *Plan, hook, runfilesDir string) {
	options := os.Getenv("NODE_OPTIONS")
	if cur, ok := plan.EnvOverrides["NODE_OPTIONS"]; ok {
		options = cur
	}
	plan.setEnv("NODE_OPTIONS", strings.TrimSpace(options+" --require "+hook))
	plan.setEnv("TS_TEST_READS_FILE", x.record)
	plan.setEnv("TS_TEST_READS_ROOT", x.workspace)
	plan.setEnv("TS_TEST_READS_RUNFILES", runfilesDir)
	plan.Supervise.StdoutToStderr = true
}

func (x *readsRun) report(w io.Writer) func(int) error {
	return func(int) error {
		data, err := os.ReadFile(x.record)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range ReadsReport(x.workspace, lines) {
			fmt.Fprintln(w, line)
		}
		return nil
	}
}

func chainPostRun(first, then func(int) error) func(int) error {
	if first == nil {
		return then
	}
	return func(code int) error {
		if err := first(code); err != nil {
			return err
		}
		return then(code)
	}
}

// ReadsReport maps the recorded workspace-relative paths to one line each,
// `path<TAB>label`, sorted and once; a directory or a missing path is dropped.
func ReadsReport(workspace string, recorded []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, rel := range recorded {
		rel = strings.TrimSpace(rel)
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		st, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(rel)))
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, rel+"\t"+dataLabel(workspace, rel))
	}
	sort.Strings(out)
	return out
}

// dataLabel is the label a data entry would take: the nearest BUILD file at or
// above the file's directory names the package.
func dataLabel(workspace, rel string) string {
	pkg := path.Dir(rel)
	for pkg != "." && !hasBuildFile(workspace, pkg) {
		pkg = path.Dir(pkg)
	}
	if pkg == "." {
		return "//:" + rel
	}
	return "//" + pkg + ":" + strings.TrimPrefix(rel, pkg+"/")
}

func hasBuildFile(workspace, pkg string) bool {
	for _, name := range []string{"BUILD.bazel", "BUILD"} {
		st, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(pkg), name))
		if err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}
