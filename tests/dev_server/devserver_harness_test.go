// The harness the dev-server tests share: start a ts_dev_server launcher the way
// `bazel run` does, and ask the running server questions over HTTP.
//
// It lives apart from the tests because two targets need it for different
// reasons -- :{target}_behaviour_test interrogates one server once,
// :{target}_hmr_latency_test edits a file under one repeatedly -- and neither
// wants the other's assertions in its binary.
package dev_server_test

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type server struct {
	cmd     *exec.Cmd
	wait    chan error
	logPath string
	port    int
}

// start runs the launcher the way `bazel run` does, in its own process group so
// the whole tree can be signalled: the launcher ignores TERM (that is how it
// survives ibazel restarts) and vite is its child.
//
// The server listens on a kernel-assigned port: on the configured one, two
// concurrent `bazel test` runs make Vite increment past the collision and one run
// then answers the other's requests. --strictPort keeps that from happening
// quietly.
func start(t *testing.T, launcher, ws, tmp string, extraArgs ...string) *server {
	t.Helper()
	port := freePort(t)
	t.Logf("serving on port %d", port)

	logPath := filepath.Join(tmp, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create %s: %v", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(launcher, append([]string{"--port", strconv.Itoa(port)}, extraArgs...)...)
	cmd.Dir = ws
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"RUNFILES_DIR="+runfilesDir(t),
		"BUILD_WORKSPACE_DIRECTORY="+ws,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", launcher, err)
	}

	srv := &server{cmd: cmd, wait: make(chan error, 1), logPath: logPath, port: port}
	go func() { srv.wait <- cmd.Wait() }()
	t.Cleanup(srv.stop)
	return srv
}

func (s *server) stop() {
	pgid := s.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGINT)
	for i := 0; i < 20; i++ {
		if done, _ := s.exited(); done {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-s.wait
}

// exited reports whether the launcher has already stopped, leaving the wait
// result in place for whoever asks next.
func (s *server) exited() (bool, error) {
	select {
	case err := <-s.wait:
		s.wait <- err
		return true, err
	default:
		return false, nil
	}
}

func (s *server) log(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(s.logPath)
	if err != nil {
		return "server log unreadable: " + err.Error()
	}
	return "--- server log ---\n" + string(raw)
}

// awaitHTTP returns the base URL that answers for probe. server.host is
// "localhost", which resolves to whichever family the sandbox offers, so the
// base URL is whichever of these three answers first.
func (s *server) awaitHTTP(t *testing.T, probe string) string {
	t.Helper()
	var lastErr error
	for i := 0; i < 120; i++ {
		for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
			base := "http://" + host + ":" + strconv.Itoa(s.port)
			_, err := httpGet(base + probe)
			if err == nil {
				return base
			}
			lastErr = err
		}
		if done, err := s.exited(); done {
			t.Fatalf("the dev server exited before it accepted a connection (%v)\n%s", err, s.log(t))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no HTTP response on port %d after 60s (last error: %v)\n%s", s.port, lastErr, s.log(t))
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// response mirrors verify.File's report-and-continue assertions over an HTTP
// body, which is not a file in the runfiles tree and so cannot go through it.
type response struct {
	url    string
	status int
	body   string
	// Plugin resolution may redirect to the dependency file.
	finalURL string
}

func get(t *testing.T, base, path string) response {
	t.Helper()
	url := base + path
	r, err := httpGet(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return r
}

// bodyOf is the served body, or "" when the request did not land. Same reason as
// `answers`: inside a poll a transport error is a "not yet".
func bodyOf(base, path string) string {
	r, err := httpGet(base + path)
	if err != nil {
		return ""
	}
	return r.body
}

// answers reports whether the server served this path, treating a transport
// error as "not yet" rather than as a failure.
//
// `get` fatals on one, which is right for a single request -- there is no server
// and the test cannot continue -- and wrong inside a poll for a server that is
// deliberately being restarted: a request landing on the socket the old process
// is closing is exactly what the poll is waiting through, and fatalling on it
// fails the test for the condition it was told to expect.
func answers(base, path string) bool {
	r, err := httpGet(base + path)
	return err == nil && r.status == 200
}

func httpGet(url string) (response, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return response{url: url}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{url: url, status: resp.StatusCode}, err
	}
	final := url
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return response{url: url, status: resp.StatusCode, body: string(body), finalURL: final}, nil
}

func (r response) contains(t *testing.T, s *server, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(r.body, want) {
			t.Errorf("%s (status %d) does not contain %q\nbody:\n%s\n%s",
				r.url, r.status, want, r.body, s.log(t))
		}
	}
}

func (r response) excludes(t *testing.T, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(r.body, bad) {
			t.Errorf("%s (status %d) contains %q, and must not\nbody:\n%s",
				r.url, r.status, bad, r.body)
		}
	}
}

func eventually(t *testing.T, done func() bool) bool {
	t.Helper()
	for i := 0; i < 60; i++ {
		if done() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is unset; the go_test target must set it", name)
	}
	return v
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// copyResolved copies a tree as regular files: a sandbox stages runfiles as
// per-file symlinks (darwin-sandbox), which a real bazel-bin never contains.
func copyResolved(t *testing.T, dst, src string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	mkdir(t, dst)
	for _, entry := range entries {
		from, to := filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())
		info, err := os.Stat(from)
		if err != nil {
			t.Fatalf("stat %s: %v", from, err)
		}
		if info.IsDir() {
			copyResolved(t, to, from)
			continue
		}
		content, err := os.ReadFile(from)
		if err != nil {
			t.Fatalf("read %s: %v", from, err)
		}
		write(t, to, string(content))
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Vite may expose dependencies through /@fs/, /@id/, or its pre-bundle cache.
func depURL(body string) string {
	m := regexp.MustCompile(`"(/(?:@(?:fs|id|oj-deps)/|bazel-bin/)[^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}
