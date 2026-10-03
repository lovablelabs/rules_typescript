package main

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ReadsFlag is the launcher's own argument: under `bazel run <test> -- --reads`
// the run reports the workspace files the tests read; the rest are vitest's.
const ReadsFlag = "--reads"

func splitReadsFlag(args []string) (bool, []string) {
	rest := slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		return a == ReadsFlag
	})
	return len(rest) < len(args), rest
}

func planVitest(
	cfg *Config, r *Resolver, plan *Plan, args []string, shard Shard,
) (_ *Plan, err error) {
	v := cfg.Vitest
	readsRequested, args := splitReadsFlag(args)
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--changed" || strings.HasPrefix(arg, "--changed=") {
			return nil, fmt.Errorf("ts_test: Vitest --changed cannot select tests in Bazel runfiles without Git source history; let Bazel reuse unchanged test targets or pass an explicit source-file filter")
		}
	}
	var reads *readsRun
	defer func() {
		if err != nil || plan.ExitEarly {
			plan.Cleanup()
		}
	}()
	if readsRequested {
		if reads, err = startReads(cfg.Label); err != nil {
			return nil, err
		}
		if err := plan.own(reads.record, false); err != nil {
			return nil, err
		}
	}

	listed, err := testFiles(r, v.TestFilesList)
	if err != nil {
		return nil, err
	}
	files := shardFiles(listed, shard)
	if len(listed) > 0 && len(files) == 0 {
		return emptyShard(plan, shard), nil
	}
	tree, err := os.MkdirTemp(os.Getenv("TEST_TMPDIR"), "ts_test_runfiles")
	if err != nil {
		return nil, err
	}
	if err := plan.own(tree, true); err != nil {
		return nil, err
	}
	if r, err = r.Stage(tree, cfg.RuntimeModules); err != nil {
		return nil, err
	}
	plan.runfilesEnv = r.Env()
	for i := range files {
		if files[i].path, err = r.Path(files[i].rlocation); err != nil {
			return nil, err
		}
	}
	filesRoot, err := stageTestRoot(files)
	if err != nil {
		return nil, err
	}
	if err := plan.own(filesRoot, true); err != nil {
		return nil, err
	}
	plan.setEnv("TS_TEST_FILES_ROOT", filesRoot)
	nodeModules, err := installNodeModules(
		plan, tree, cfg.Workspace, v.NodeModules)
	if err != nil {
		return nil, err
	}
	if err := stageFiles(r, tree, v.Stage); err != nil {
		return nil, err
	}
	if err := placeNpmContexts(r, v.NpmContexts, cfg.RuntimeModules); err != nil {
		return nil, err
	}
	configFile := filepath.Join(tree, filepath.FromSlash(v.ConfigFile))
	plan.Dir = filepath.Join(
		filepath.Dir(configFile), filepath.FromSlash(v.RootRel))

	flags := []string{"run", "--config", configFile}
	flags = append(flags, coverageFlags()...)
	flags = append(flags, args...)

	vitestBin, err := resolveVitest(v, nodeModules)
	if err != nil {
		return nil, err
	}
	runtime, err := runtimeCommand(cfg, r)
	if err != nil {
		return nil, err
	}
	argv := append(runtime, vitestBin)
	plan.Argv = append(argv, flags...)
	plan.UseExec = false
	plan.PostRun = writeCoverage(cfg.Workspace, tree, plan.Dir)
	if reads != nil {
		hook, err := r.Path(v.ReadsHook)
		if err != nil {
			return nil, err
		}
		reads.install(plan, hook, r.Dir())
		plan.PostRun = chainPostRun(plan.PostRun, reads.report(os.Stdout))
	}
	return plan, nil
}

// stageFiles writes each entry as a regular file under the tree: a config's
// __dirname and its bare-import walk-up then start at its package path.
func stageFiles(r *Resolver, tree string, stage map[string]string) error {
	root, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer root.Close()
	paths := slices.Sorted(maps.Keys(stage))
	for _, from := range paths {
		dst := filepath.FromSlash(stage[from])
		if !filepath.IsLocal(dst) || dst == "." {
			return fmt.Errorf("ts_test: config staging destination %q is outside the runfiles tree", stage[from])
		}
		parent := "."
		for _, part := range strings.Split(filepath.Dir(dst), string(filepath.Separator)) {
			parent = filepath.Join(parent, part)
			info, err := root.Lstat(parent)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("ts_test: cannot stage config %q beneath non-directory or symlink entry %q; expose its runfiles individually", stage[from], parent)
			}
		}
	}
	for _, from := range paths {
		src, err := r.Path(from)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("ts_test: reading %s: %w", from, err)
		}
		dst := filepath.FromSlash(stage[from])
		if err := root.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := root.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := root.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// stageTestRoot links the program's files, at their runfiles paths, into a
// root of their own: vitest's walk is the size of the run, not of the tree.
func stageTestRoot(files []testFile) (_ string, err error) {
	root, err := os.MkdirTemp(os.Getenv("TEST_TMPDIR"), "ts_test_root")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()
	for _, f := range files {
		link := filepath.Join(root, filepath.FromSlash(f.rlocation))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return "", err
		}
		_ = os.Remove(link)
		if err := os.Symlink(f.path, link); err != nil {
			return "", err
		}
	}
	return root, nil
}

// resolveVitest finds vitest's bin entry under the first importer on the
// chain that links it, pnpm's walk up from the test's package.
func resolveVitest(v *VitestConfig, nodeModules []string) (string, error) {
	if v.VitestInTree != "" {
		for _, dir := range nodeModules {
			p := filepath.Join(dir, filepath.FromSlash(v.VitestInTree))
			if fileExists(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf(
		"ts_test: vitest is in the node_modules of no importer on the chain %q; "+
			"add the hub label to deps and declare it in the importer's "+
			"package.json", nodeModules)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// testFile is one declared test file: the runfiles path the rule wrote and the
// filesystem path it resolves to.
type testFile struct {
	rlocation string
	path      string
}

// testFiles reads the generated list of compiled test files, one runfiles
// path per line.
func testFiles(r *Resolver, listPath string) ([]testFile, error) {
	list, err := r.Path(listPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(list)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := []testFile{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		abs, err := r.Path(line)
		if err != nil {
			return nil, err
		}
		out = append(out, testFile{rlocation: line, path: abs})
	}
	return out, scanner.Err()
}

func shardFiles(files []testFile, shard Shard) []testFile {
	files = slices.Clone(files)
	slices.SortFunc(files, func(a, b testFile) int {
		return strings.Compare(a.rlocation, b.rlocation)
	})
	out := []testFile{}
	for i, f := range files {
		if i%shard.Total == shard.Index {
			out = append(out, f)
		}
	}
	return out
}

func coverageFlags() []string {
	dir := os.Getenv("COVERAGE_DIR")
	if dir == "" {
		return nil
	}
	return []string{
		"--coverage.enabled", "true",
		"--coverage.reporter", "lcov",
		"--coverage.reportsDirectory", filepath.Join(dir, "vitest"),
	}
}

// writeCoverage is the run's post-step under `bazel coverage`: vitest's lcov,
// its paths made workspace-relative, as the .dat the rule's merger reads.
func writeCoverage(workspace, runDir, root string) func(int) error {
	dir := os.Getenv("COVERAGE_DIR")
	if dir == "" {
		return nil
	}
	return func(code int) error {
		if code != 0 {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(dir, "vitest", "lcov.info"))
		if err != nil {
			return fmt.Errorf("ts_test: vitest wrote no coverage report: %w", err)
		}
		lcov := RewriteLcov(data, workspace, runDir, root)
		return os.WriteFile(filepath.Join(dir, "vitest.dat"), lcov, 0o644)
	}
}

// RewriteLcov turns the paths vitest reports, relative to its root, into the
// workspace-relative ones the coverage manifest is matched against.
func RewriteLcov(data []byte, workspace, runDir, root string) []byte {
	tree := ""
	if runDir != "" && workspace != "" {
		tree = filepath.ToSlash(runDir) + "/" + workspace + "/"
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		rest, isSF := strings.CutPrefix(line, "SF:")
		if !isSF {
			continue
		}
		p := rest
		if !filepath.IsAbs(p) && root != "" {
			p = filepath.Join(root, p)
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if tree != "" && strings.HasPrefix(p, tree) {
			lines[i] = "SF:" + strings.TrimPrefix(p, tree)
		} else if pkg, ok := packagePathUnderBazelOut(p); ok {
			lines[i] = "SF:" + pkg
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// packagePathUnderBazelOut recovers the package path of a build output istanbul
// named by its execroot realpath: a module a pool resolved past its symlink.
func packagePathUnderBazelOut(p string) (string, bool) {
	i := strings.LastIndex(p, "/bazel-out/")
	if i < 0 {
		return "", false
	}
	parts := strings.SplitN(p[i+len("/bazel-out/"):], "/", 3)
	if len(parts) != 3 || parts[1] != "bin" || strings.Contains(parts[2], ".runfiles/") {
		return "", false
	}
	return parts[2], true
}
