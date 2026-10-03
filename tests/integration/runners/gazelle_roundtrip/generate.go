package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/mikn/rules_typescript/tests/integration/harness"
)

func foreignExportsText() (string, error) {
	file, err := rule.LoadData("BUILD.bazel", "foreign_fixtures", []byte(`exports_files(["value.json", "a.ts", "nested/b.ts", "companion.mjs"], visibility = ["//foreign_json:__pkg__"])
`))
	if err != nil {
		return "", fmt.Errorf("cannot load foreign source exports: %w", err)
	}
	return string(file.Format()), nil
}

type stager struct {
	root string
	err  error
}

func (s *stager) read(rel string) string {
	text, err := os.ReadFile(filepath.Join(s.root, rel))
	s.err = errors.Join(s.err, err)
	return string(text)
}

func (s *stager) write(rel, content string) {
	path := filepath.Join(s.root, rel)
	s.err = errors.Join(s.err, os.MkdirAll(filepath.Dir(path), 0o755), os.WriteFile(path, []byte(content), 0o644))
}

// stageFixture writes the hand-written packages the first Gazelle pass merges into.
func stageFixture(root, workersLock string) error {
	s := &stager{root: root}
	// Written here, not shipped: a BUILD file under a --deleted_packages
	// entry is the OUTER workspace's, and glob_workspace_files stops there.
	s.write("src/i18n/BUILD.bazel", cataloguePackage)
	s.write("src/locales/BUILD.bazel", outDirCodegenPackage)
	s.write("generated_worker/BUILD.bazel", generatedWorkerPackage)
	s.write("shared_program/BUILD.bazel", sharedProgramPackage)
	s.write("worker/src/BUILD.bazel", staleWorkerSrcPackage)
	s.write("wrangler_lock/BUILD.bazel", wranglerLock)
	s.write("pooled/wrangler.source.jsonc", s.read("pooled/wrangler.jsonc"))
	s.err = errors.Join(s.err, os.Remove(filepath.Join(root, "pooled/wrangler.jsonc")))
	s.write("pooled/BUILD.bazel", `genrule(
    name = "wrangler",
    srcs = ["wrangler.source.jsonc"],
    outs = ["wrangler.jsonc"],
    cmd = "cp $(location wrangler.source.jsonc) $@",
)
`)
	s.write("BUILD.bazel", s.read("BUILD.bazel")+`
sh_binary(
    name = "json_gen",
    srcs = ["tools/json_gen.sh"],
    visibility = ["//visibility:public"],
)
`)
	// wrangler, for generated_worker's ts_codegen, is in tests/workers' lockfile.
	lock, err := os.ReadFile(workersLock)
	s.err = errors.Join(s.err, err)
	s.write("wrangler_lock/pnpm-lock.yaml", string(lock))
	exports, err := foreignExportsText()
	s.err = errors.Join(s.err, err)
	s.write("foreign_fixtures/BUILD.bazel", exports)
	return s.err
}

// generateWorkspace is the build action behind gazelle_roundtrip_workspace:
// the staged fixture, installed from the lockfile's tarballs and through one
// Gazelle pass, with the install and Gazelle's output beside it.
func generateWorkspace(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	files := flags.String("files", "", "the fixture's files, one per line")
	fixture := flags.String("fixture", "", "the fixture directory those files are under")
	workersLock := flags.String("workers_lock", "", "tests/workers' pnpm-lock.yaml")
	pnpm := flags.String("pnpm", "", "the pnpm binary")
	registry := flags.String("registry", "", "a file at the root of the tarball registry")
	gazelle := flags.String("gazelle", "", "the gazelle() runner")
	out := flags.String("out", "", "the workspace directory to write")
	install := flags.String("install", "", "the install archive to write")
	logFile := flags.String("log", "", "the Gazelle log to write")
	if err := flags.Parse(args); err != nil {
		return err
	}
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err := copyFixture(*files, *fixture, root); err != nil {
		return err
	}
	if err := stageFixture(root, *workersLock); err != nil {
		return err
	}

	scratch := filepath.Join(root, ".generate")
	store := filepath.Join(scratch, "store")
	url, stop, err := harness.ServeRegistry(filepath.Dir(*registry), "")
	if err != nil {
		return err
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "npm_config_registry=" + url, "npm_config_update_notifier=false",
		"npm_config_cache_dir=" + filepath.Join(scratch, "cache"), "npm_config_state_dir=" + filepath.Join(scratch, "state")}
	for _, name := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		env = append(env, name+"="+filepath.Join(scratch, strings.ToLower(name)))
	}
	pnpmBinary, err := filepath.Abs(*pnpm)
	if err != nil {
		return err
	}
	_, err = run(root, env, pnpmBinary, "install", "--frozen-lockfile", "--prefer-offline", "--ignore-scripts", "--store-dir", store)
	stop()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(scratch); err != nil {
		return err
	}

	// As `bazel run //:gazelle -- -ts_verbose` runs the consumer's runner: from its runfiles, at the workspace.
	binary, err := filepath.Abs(*gazelle)
	if err != nil {
		return err
	}
	output, err := run(binary+".runfiles/_main", []string{"PATH=" + os.Getenv("PATH"), "BUILD_WORKSPACE_DIRECTORY=" + root},
		binary, "-ts_verbose")
	if err != nil {
		return err
	}
	output = strings.NewReplacer(root, "{WORKSPACE}", execroot, "{EXECROOT}").Replace(output)
	if err := os.WriteFile(*logFile, []byte(output), 0o644); err != nil {
		return err
	}
	return harness.PackInstall(root, *install, store, url)
}

func copyFixture(list, fixture, root string) error {
	in, err := os.Open(list)
	if err != nil {
		return err
	}
	defer in.Close()
	lines := bufio.NewScanner(in)
	for lines.Scan() {
		rel, ok := strings.CutPrefix(lines.Text(), fixture+"/")
		if !ok {
			return fmt.Errorf("%s is not under %s", lines.Text(), fixture)
		}
		info, err := os.Stat(lines.Text())
		if err != nil {
			return err
		}
		text, err := os.ReadFile(lines.Text())
		if err != nil {
			return err
		}
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, text, info.Mode().Perm()|0o200); err != nil {
			return err
		}
	}
	return lines.Err()
}

func run(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", filepath.Base(name), strings.Join(args, " "), err, output)
	}
	return string(output), nil
}
