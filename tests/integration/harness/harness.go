// Package harness stages a child Bazel workspace and drives the nested Bazel
// that an integration test asserts against.
package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/mikn/rules_typescript/tests/hmrsocket"
)

// Config names the child workspace under the checkout the nested Bazel runs
// over; with no WorkspaceRel it runs over the checkout itself.
type Config struct {
	Name         string
	WorkspaceRel string
	Renames      map[string]string
}

type IT struct {
	Name         string
	RulesTSRoot  string
	WorkspaceDir string
	OutputBase   string

	bazel      string
	staged     string
	scratchDir string
	bazelrc    string
	bazelBin   string
	registry   string
	stops      []func()

	nestedCount int
	nestedTime  time.Duration
}

type Log struct {
	Path string
	Text string
}

type failure struct{ msg string }

func Run(cfg Config, body func(*IT)) {
	it, err := start(cfg)
	if err != nil {
		if it != nil {
			it.cleanup()
		}
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	started := time.Now()
	code := invoke(it, body)
	fmt.Printf("CHECK %6.1fs total (%d nested bazel, %.1fs)\n", time.Since(started).Seconds(),
		it.nestedCount, it.nestedTime.Seconds())
	it.cleanup()
	if code == 0 {
		fmt.Println("ALL PASSED")
	}
	os.Exit(code)
}

func invoke(it *IT, body func(*IT)) (code int) {
	defer func() {
		switch r := recover().(type) {
		case nil:
		case failure:
			fmt.Fprintf(os.Stderr, "FAIL: %s\n", r.msg)
			code = 1
		default:
			fmt.Fprintf(os.Stderr, "FAIL: panic: %v\n%s\n", r, debug.Stack())
			code = 1
		}
	}()
	body(it)
	return 0
}

func start(cfg Config) (*IT, error) {
	bazel := os.Getenv("BIT_BAZEL_BINARY")
	if bazel == "" {
		return nil, errors.New("BIT_BAZEL_BINARY not set")
	}
	workspaceSrc := os.Getenv("BIT_WORKSPACE_DIR")
	if workspaceSrc == "" {
		return nil, errors.New("BIT_WORKSPACE_DIR not set")
	}
	fmt.Printf("INFO: bazel           = %s\n", bazel)
	fmt.Printf("INFO: workspace_dir   = %s\n", workspaceSrc)

	root, via, err := rulesTSRoot(workspaceSrc, cfg.WorkspaceRel)
	if err != nil {
		return nil, err
	}
	fmt.Printf("INFO: rules_ts_root   = %s (via %s)\n", root, via)

	base, err := runRoot(cfg.Name, root)
	if err != nil {
		return nil, err
	}
	fmt.Printf("INFO: run_root        = %s\n", base)
	it := &IT{
		Name:         cfg.Name,
		RulesTSRoot:  root,
		WorkspaceDir: root,
		OutputBase:   filepath.Join(base, "output_base"),
		bazel:        bazel,
		scratchDir:   filepath.Join(base, "scratch"),
	}
	if cfg.WorkspaceRel != "" {
		it.staged = filepath.Join(base, "workspace")
		it.WorkspaceDir = it.staged
	}
	return it, it.prepare(cfg, workspaceSrc)
}

func (it *IT) prepare(cfg Config, workspaceSrc string) error {
	for _, dir := range it.scratchDirs() {
		makeWritable(dir)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(it.OutputBase, 0o755); err != nil {
		return err
	}
	if it.staged == "" {
		return it.shareRepositoryCache()
	}
	if err := stage(workspaceSrc, it.WorkspaceDir); err != nil {
		return err
	}
	module := filepath.Join(it.WorkspaceDir, "MODULE.bazel")
	text, err := os.ReadFile(module)
	if err != nil {
		return err
	}
	if err := os.WriteFile(module, []byte(strings.ReplaceAll(string(text), "{RULES_TS_ROOT}", it.RulesTSRoot)), 0o644); err != nil {
		return err
	}
	for from, to := range cfg.Renames {
		if err := os.Rename(filepath.Join(it.WorkspaceDir, from), filepath.Join(it.WorkspaceDir, to)); err != nil {
			return err
		}
	}
	return it.shareRepositoryCache()
}

func (it *IT) scratchDirs() []string {
	if it.staged == "" {
		return []string{it.scratchDir}
	}
	return []string{it.staged, it.scratchDir}
}

// Each test gets its own output base, so without a shared repository cache all
// of them re-fetch the whole BCR registry at once and the concurrent DNS
// lookups start failing ("Unknown host: bcr.bazel.build") on a different
// subset of tests each run.
// Downloads and action results are both shared across the suite. Each test has
// its own output base, so without the disk cache none of them reuses an action
// a sibling already ran: sequentially from cold the same three tests took 188s,
// 88s and 21s with it.
func (it *IT) shareRepositoryCache() error {
	repo := filepath.Join(cacheRoot(), "repository_cache")
	disk := filepath.Join(cacheRoot(), "disk_cache")
	for _, dir := range []string{repo, disk, bazeliskHome()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	lines := "common --repository_cache=" + repo + "\n" +
		"common --disk_cache=" + disk + "\n"
	if it.staged == "" {
		// The checkout keeps its .bazelrc and the outer build's bazel-* links.
		it.bazelrc = filepath.Join(it.scratchDir, "bazelrc")
		lines += "build --experimental_convenience_symlinks=ignore\n"
		return os.WriteFile(it.bazelrc, []byte(lines), 0o644)
	}
	rc := filepath.Join(it.staged, ".bazelrc")
	f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n%s", lines)
	return err
}

// The persistent root: RULES_TS_IT_SCRATCH is the name ci.yml sets. Never
// TEST_TMPDIR: the outer Bazel clears it on each `bazel test` (docs/CI_CD.md).
func cacheRoot() string {
	if dir := os.Getenv("RULES_TS_IT_SCRATCH"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "rules_typescript_it")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".cache", "rules_typescript_it")
	}
	return filepath.Join(os.TempDir(), "rules_typescript_it")
}

// bazelisk defaults BAZELISK_HOME to $PWD, the WorkspaceDir prepare() recreates
// per run: unset, every test fetches Bazel from releases.bazel.build again.
func bazeliskHome() string {
	if dir := os.Getenv("BAZELISK_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(cacheRoot(), "bazelisk")
}

// Under `bazel test` the run root is TEST_TMPDIR. The fallback is keyed by
// checkout and test, not os.MkdirTemp: the next run overwrites a killed run's.
func runRoot(name, checkout string) (string, error) {
	if dir := os.Getenv("TEST_TMPDIR"); dir != "" {
		return dir, nil
	}
	sum := sha256.Sum256([]byte(checkout))
	dir := filepath.Join(cacheRoot(), "runs", hex.EncodeToString(sum[:6]), name)
	return dir, os.MkdirAll(dir, 0o755)
}

func rulesTSRoot(workspaceSrc, workspaceRel string) (root, via string, err error) {
	tried := []string{}
	if rf, err := runfiles.New(); err == nil {
		if path, err := rf.Rlocation("_main/MODULE.bazel"); err == nil {
			tried = append(tried, path)
			if root, err := checkoutRoot(path); err == nil {
				return root, "runfiles", nil
			}
		}
	}
	if workspaceRel == "" {
		return "", "", fmt.Errorf("no rules_typescript checkout in the runfiles "+
			"(tried %s)", strings.Join(tried, ", "))
	}
	trimmed := strings.TrimSuffix(workspaceSrc, "/"+workspaceRel)
	if trimmed == workspaceSrc {
		return "", "", fmt.Errorf("BIT_WORKSPACE_DIR %q does not end with %q", workspaceSrc, workspaceRel)
	}
	path := filepath.Join(trimmed, "MODULE.bazel")
	tried = append(tried, path)
	root, err = checkoutRoot(path)
	if err != nil {
		return "", "", fmt.Errorf("cannot locate the rules_typescript checkout (tried %s): %w", strings.Join(tried, ", "), err)
	}
	return root, "BIT_WORKSPACE_DIR", nil
}

func checkoutRoot(moduleInRunfiles string) (string, error) {
	resolved, err := filepath.EvalSymlinks(moduleInRunfiles)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(resolved)
	if _, err := os.Stat(filepath.Join(root, "oxc_cli")); err != nil {
		return "", fmt.Errorf("%s has no oxc_cli directory", root)
	}
	module, err := os.ReadFile(filepath.Join(root, "MODULE.bazel"))
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(module), `"rules_typescript"`) {
		return "", fmt.Errorf("%s/MODULE.bazel does not declare rules_typescript", root)
	}
	return root, nil
}

func stage(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		info, err := os.Stat(from)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(to, 0o755); err != nil {
				return err
			}
			if err := stage(from, to); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func makeWritable(dir string) {
	filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		extra := os.FileMode(0o200)
		if entry.IsDir() {
			extra = 0o300
		}
		os.Chmod(path, info.Mode().Perm()|extra)
		return nil
	})
}

// The output base is kept: under `bazel test` the next run clears TEST_TMPDIR
// anyway, and outside it the next run of this test reuses it in place.
func (it *IT) cleanup() {
	for i := len(it.stops) - 1; i >= 0; i-- {
		it.stops[i]()
	}
	shutdown := exec.Command(it.bazel, append(it.startup(), "shutdown")...)
	shutdown.Env = nestedEnv()
	shutdown.Run()
	for _, dir := range it.scratchDirs() {
		makeWritable(dir)
		os.RemoveAll(dir)
	}
}

func (it *IT) OnCleanup(stop func()) {
	it.stops = append(it.stops, stop)
}

func (it *IT) Pass(format string, a ...any) {
	fmt.Printf("PASS: "+format+"\n", a...)
}

func (it *IT) Fail(format string, a ...any) {
	panic(failure{msg: fmt.Sprintf(format, a...)})
}

func (it *IT) Path(rel ...string) string {
	return filepath.Join(append([]string{it.WorkspaceDir}, rel...)...)
}

func (it *IT) Bin(rel ...string) string {
	return filepath.Join(append([]string{it.BazelBin()}, rel...)...)
}

func (it *IT) Scratch(rel ...string) string {
	path := filepath.Join(append([]string{it.scratchDir}, rel...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		it.Fail("cannot create %s: %v", filepath.Dir(path), err)
	}
	return path
}

// Inherited TEST_TMPDIR makes nested Bazel reject its repo cache inside the outer execroot.
func nestedEnv() []string {
	env := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "TEST_TMPDIR=") || strings.HasPrefix(entry, "BAZELISK_HOME=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "BAZELISK_HOME="+bazeliskHome())
}

// No user or system rc: setup-bazel's ~/.bazelrc `build --disk_cache` outranks
// the `common` one above and sent every nested action to a cache CI never saves.
func (it *IT) startup() []string {
	opts := []string{"--nosystem_rc", "--nohome_rc", "--output_base=" + it.OutputBase}
	if it.bazelrc != "" {
		opts = append(opts, "--bazelrc="+it.bazelrc)
	}
	return opts
}

// BazelExecutable is the nested Bazel as one executable, for a tool that
// takes it through an environment variable (BAZEL=...).
func (it *IT) BazelExecutable() string {
	path := it.Scratch("bazel")
	words := []string{}
	for _, word := range append([]string{it.bazel}, it.startup()...) {
		words = append(words, "'"+strings.ReplaceAll(word, "'", `'\''`)+"'")
	}
	script := "#!/usr/bin/env bash\nexec " + strings.Join(words, " ") + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		it.Fail("cannot write %s: %v", path, err)
	}
	return path
}

func (it *IT) command(args []string) *exec.Cmd {
	cmd := exec.Command(it.bazel, append(it.startup(), args...)...)
	cmd.Dir = it.WorkspaceDir
	cmd.Env = nestedEnv()
	return cmd
}

// Bazel's own summary of where each nested invocation's time went.
var nestedSummaryLine = regexp.MustCompile(`^INFO: (\d+ process(es)?: .*|Analyzed .*|Elapsed time: .*)$`)

type summaryWriter struct {
	out     io.Writer
	partial []byte
	found   []string
}

func (w *summaryWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.partial[:i]), "\r")
		if m := nestedSummaryLine.FindStringSubmatch(line); m != nil {
			w.found = append(w.found, m[1])
		}
		w.partial = w.partial[i+1:]
	}
	return w.out.Write(p)
}

// run executes a nested Bazel and prints one NESTED line with its wall time and
// cache summary, so a slow check shows which invocation and which actions cost it.
func (it *IT) run(cmd *exec.Cmd, args []string) error {
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	summary := &summaryWriter{out: stderr}
	// One writer for both keeps exec from copying into a shared builder from two goroutines.
	if cmd.Stdout == cmd.Stderr {
		cmd.Stdout = summary
	}
	cmd.Stderr = summary
	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)
	it.nestedCount++
	it.nestedTime += elapsed
	verb := strings.Join(args, " ")
	if len(verb) > 120 {
		verb = verb[:120] + "..."
	}
	fmt.Printf("NESTED %6.1fs bazel %s | %s\n", elapsed.Seconds(), verb, strings.Join(summary.found, " | "))
	return err
}

// Check runs one named check and prints its wall time and nested Bazel share.
func (it *IT) Check(name string, body func()) {
	started, count, nested := time.Now(), it.nestedCount, it.nestedTime
	body()
	fmt.Printf("CHECK %6.1fs %s (%d nested bazel, %.1fs)\n", time.Since(started).Seconds(), name,
		it.nestedCount-count, (it.nestedTime - nested).Seconds())
}

func (it *IT) Bazel(args ...string) error {
	fmt.Printf("INFO: bazel %s\n", strings.Join(args, " "))
	cmd := it.command(args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return it.run(cmd, args)
}

func (it *IT) MustBazel(args ...string) {
	if err := it.Bazel(args...); err != nil {
		it.Fail("bazel %s exited non-zero: %v", strings.Join(args, " "), err)
	}
}

func (it *IT) BazelStdout(args ...string) string {
	fmt.Printf("INFO: bazel %s\n", strings.Join(args, " "))
	cmd := it.command(args)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	if err := it.run(cmd, args); err != nil {
		it.Fail("bazel %s exited non-zero: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func (it *IT) BazelLog(logName string, args ...string) (*Log, error) {
	fmt.Printf("INFO: bazel %s\n", strings.Join(args, " "))
	cmd := it.command(args)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out
	err := it.run(cmd, args)
	log := &Log{Path: it.Scratch(logName), Text: out.String()}
	if writeErr := os.WriteFile(log.Path, []byte(log.Text), 0o644); writeErr != nil {
		it.Fail("cannot write %s: %v", log.Path, writeErr)
	}
	return log, err
}

// Install runs the workspace's ts_pnpm over its lockfile: Gazelle lists that
// tree. Its store is under the cache root, so a warm run fetches nothing.
func (it *IT) Install() {
	store := filepath.Join(cacheRoot(), "pnpm")
	args := []string{"run", "//:pnpm", "--", "install", "--frozen-lockfile",
		"--prefer-offline", "--ignore-scripts",
		"--store-dir", filepath.Join(store, "store")}
	fmt.Printf("INFO: bazel %s\n", strings.Join(args, " "))
	cmd := it.command(args)
	cmd.Env = append(cmd.Env,
		"npm_config_cache_dir="+filepath.Join(store, "cache"),
		"npm_config_state_dir="+filepath.Join(store, "state"))
	if it.registry != "" {
		cmd.Env = append(cmd.Env, "npm_config_registry="+it.registry)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := it.run(cmd, args); err != nil {
		it.Fail("pnpm install exited non-zero: %v", err)
	}
	it.RequireFile(it.Path("node_modules", ".modules.yaml"),
		"pnpm install left no node_modules/.modules.yaml at the workspace root")
}

func (it *IT) BazelBin() string {
	if it.bazelBin == "" {
		cmd := it.command([]string{"info", "bazel-bin"})
		out := &strings.Builder{}
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			it.Fail("bazel info bazel-bin failed: %v", err)
		}
		it.bazelBin = strings.TrimSpace(out.String())
		if it.bazelBin == "" {
			it.Fail("bazel info bazel-bin printed nothing")
		}
	}
	return it.bazelBin
}

func (l *Log) Contains(text string) bool {
	return strings.Contains(l.Text, text)
}

func (l *Log) Matches(pattern string) bool {
	return regexp.MustCompile(pattern).MatchString(l.Text)
}

func (l *Log) Lines() []string {
	return strings.Split(l.Text, "\n")
}

func (l *Log) Dump() {
	fmt.Fprintln(os.Stderr, "--- build output ---")
	fmt.Fprintln(os.Stderr, l.Text)
}

func (l *Log) DumpTail(lines int) {
	all := l.Lines()
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	fmt.Fprintln(os.Stderr, "--- build output ---")
	fmt.Fprintln(os.Stderr, strings.Join(all, "\n"))
}

// Runfile resolves a path in the TEST's own runfiles -- the outer repository's,
// which is where a tool the assertions need (a Node binary, say) comes from. The
// child workspace has its own outputs and knows nothing about these.
func (it *IT) Runfile(rel string) string {
	rf, err := runfiles.New()
	if err != nil {
		it.Fail("cannot open runfiles: %v", err)
	}
	path, err := rf.Rlocation(rel)
	if err != nil {
		it.Fail("cannot resolve runfile %s: %v", rel, err)
	}
	return path
}

// Exec runs a command outside Bazel, in the workspace under the nested Bazel's
// environment plus env; its output is kept beside the nested build logs.
func (it *IT) Exec(
	logName string, env []string, name string, args ...string,
) (*Log, error) {
	fmt.Printf("INFO: %s %s %s\n", strings.Join(env, " "), name,
		strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir = it.WorkspaceDir
	cmd.Env = append(nestedEnv(), env...)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	log := &Log{Path: it.Scratch(logName), Text: out.String()}
	if writeErr := os.WriteFile(log.Path, []byte(log.Text), 0o644); writeErr != nil {
		it.Fail("cannot write %s: %v", log.Path, writeErr)
	}
	return log, err
}

// Server is a `bazel run` target left running, and the base URL it answers on.
type Server struct {
	Base string
	Log  string

	cmd  *exec.Cmd
	out  *strings.Builder
	stop func()
	once sync.Once
}

// Stop shuts the server down early, for a test that needs to look at what it
// left behind. Stopping twice is safe; cleanup calls it again.
func (s *Server) Stop() {
	s.once.Do(s.stop)
}

// Output is everything the server has written so far. A dev server reports most
// of what goes wrong on stderr and still answers 200, so an assertion that only
// reads the response body can pass over a broken one.
func (s *Server) Output() string { return s.out.String() }

// Serve starts a `bazel run` target on a free port and waits for it to answer,
// returning the base URL. The target is stopped when the test ends.
//
// The launcher ignores SIGTERM (that is how a dev server survives an ibazel
// rebuild) and bazel is its parent, so the whole process group is signalled.
func (it *IT) Serve(logName, target string, args ...string) *Server {
	port := freePort(it)
	run := append([]string{"run", target, "--", "--port", strconv.Itoa(port)}, args...)
	fmt.Printf("INFO: bazel %s\n", strings.Join(run, " "))
	cmd := it.command(run)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		it.Fail("cannot start %s: %v", target, err)
	}

	srv := &Server{
		Base: fmt.Sprintf("http://localhost:%d", port),
		Log:  it.Scratch(logName),
		cmd:  cmd,
		out:  out,
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	srv.stop = func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		_ = os.WriteFile(srv.Log, []byte(out.String()), 0o644)
	}
	it.OnCleanup(srv.Stop)

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			_ = os.WriteFile(srv.Log, []byte(out.String()), 0o644)
			it.Fail("%s exited before it answered (%v):\n%s", target, err, out.String())
		default:
		}
		if resp, err := http.Get(srv.Base + "/"); err == nil {
			resp.Body.Close()
			return srv
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = os.WriteFile(srv.Log, []byte(out.String()), 0o644)
	it.Fail("%s never answered on %s:\n%s", target, srv.Base, out.String())
	return nil
}

// Get fetches a path from the running server and returns the status and body.
func (it *IT) Get(srv *Server, path string) (int, string) {
	resp, err := http.Get(srv.Base + path)
	if err != nil {
		it.Fail("GET %s: %v\n%s", path, err, srv.Output())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		it.Fail("reading GET %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// DialHMR opens an HMR client against a running server, so a test can ask
// whether an edit was published rather than only whether the next request
// happens to render the new bytes.
func (it *IT) DialHMR(srv *Server, path, protocol string) *hmrsocket.Socket {
	sock, err := hmrsocket.Dial(strings.TrimPrefix(srv.Base, "http://"), path, protocol)
	if err != nil {
		it.Fail("%v\n%s", err, srv.Output())
	}
	it.OnCleanup(sock.Close)
	return sock
}

// freePort asks the kernel for one. The configured port would collide between
// two concurrent runs, and Vite silently increments past a collision -- so one
// run would answer the other's requests.
func freePort(it *IT) int {
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		it.Fail("cannot reserve a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Glob returns the names of the entries in dir whose name starts with prefix and
// ends with suffix. Rollup names every chunk with a content hash, so the two ends
// are all a test can pin.
func (it *IT) Glob(dir, prefix, suffix string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		it.Fail("cannot read %s: %v", dir, err)
	}
	var found []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			found = append(found, name)
		}
	}
	return found
}

func (it *IT) Read(path string) string {
	text, err := os.ReadFile(path)
	if err != nil {
		it.Fail("cannot read %s: %v", path, err)
	}
	return string(text)
}

func (it *IT) Write(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		it.Fail("cannot create the directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		it.Fail("cannot write %s: %v", path, err)
	}
}

func (it *IT) Replace(path, old, new string) {
	text := it.Read(path)
	if !strings.Contains(text, old) {
		it.Fail("%s does not contain %q, so the edit did not apply", path, old)
	}
	it.Write(path, strings.ReplaceAll(text, old, new))
}

func (it *IT) RequireFile(path, format string, a ...any) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireNoFile(path, format string, a ...any) {
	if _, err := os.Stat(path); err == nil {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireExecutable(path, format string, a ...any) {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o111 == 0 {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireDir(path, format string, a ...any) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireNoDir(path, format string, a ...any) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireContains(path, text, format string, a ...any) {
	if !strings.Contains(it.Read(path), text) {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireNotContains(path, text, format string, a ...any) {
	if strings.Contains(it.Read(path), text) {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireMatches(path, pattern, format string, a ...any) {
	if !regexp.MustCompile(pattern).MatchString(it.Read(path)) {
		it.Fail(format, a...)
	}
}

func (it *IT) RequireNotMatches(path, pattern, format string, a ...any) {
	if regexp.MustCompile(pattern).MatchString(it.Read(path)) {
		it.Fail(format, a...)
	}
}

func (it *IT) Dump(path string) {
	fmt.Printf("--- %s ---\n%s------------------------\n", path, it.Read(path))
}

func (it *IT) Contains(path, text string) bool {
	return strings.Contains(it.Read(path), text)
}

func (it *IT) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
