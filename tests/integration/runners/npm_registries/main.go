package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mikn/rules_typescript/tests/integration/harness"
)

const (
	acmeToken    = "acme-integration-test-token"
	lumenToken   = "lumen-integration-test-token"
	acmeTarball  = "@acme/greeter/-/greeter-1.0.0.tgz"
	lumenTarball = "@lumen/greeter/-/greeter-1.0.0.tgz"
	declarations = "--output_groups=+declarations"
)

var lumenFetch = regexp.MustCompile(
	`Error downloading \[([^\]]*@lumen[^\]]*)\]`)

type registry struct {
	root    string
	token   string
	mu      sync.Mutex
	entries []string
}

func (r *registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	auth := req.Header.Get("Authorization")
	if auth == "" {
		auth = "-"
	}
	reply := func(status int, body []byte) {
		entry := fmt.Sprintf("%d %s %s", status, req.URL.RequestURI(), auth)
		r.mu.Lock()
		r.entries = append(r.entries, entry)
		r.mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		w.Write(body)
	}
	if auth != "Bearer "+r.token {
		reply(http.StatusUnauthorized, nil)
		return
	}
	target := filepath.Join(r.root, strings.TrimPrefix(req.URL.Path, "/"))
	if !strings.HasPrefix(target, r.root+string(os.PathSeparator)) {
		reply(http.StatusNotFound, nil)
		return
	}
	body, err := os.ReadFile(target)
	if err != nil {
		reply(http.StatusNotFound, nil)
		return
	}
	reply(http.StatusOK, body)
}

func (r *registry) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.entries, "\n")
}

func (r *registry) saw(line string) bool {
	for _, entry := range strings.Split(r.log(), "\n") {
		if entry == line {
			return true
		}
	}
	return false
}

func (r *registry) dump(name string) {
	fmt.Fprintf(os.Stderr, "--- %s request log ---\n%s\n", name, r.log())
}

// The two scoped imports' hub labels: what the fetches through the two
// registries have to answer.
const consumerPackage = `load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "consumer",
    emit = True,
    srcs = ["greeting.ts"],
    node_modules = "//:node_modules",
    deps = [
        "@npm//:acme_greeter",
        "@npm//:lumen_greeter",
    ],
)
`

const greeterJS = "export function greet(who) {\n" +
	"  return `hello ${who}`;\n}\n"

const greeterDTS = "export declare function greet(who: string): string;\n"

func writeTarball(it *harness.IT, path string, files map[string]string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		it.Fail("cannot create %s: %v", filepath.Dir(path), err)
	}
	out, err := os.Create(path)
	if err != nil {
		it.Fail("cannot create %s: %v", path, err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	archive := tar.NewWriter(gz)
	for name, content := range files {
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := archive.WriteHeader(header); err != nil {
			it.Fail("cannot add %s to %s: %v", name, path, err)
		}
		if _, err := archive.Write([]byte(content)); err != nil {
			it.Fail("cannot add %s to %s: %v", name, path, err)
		}
	}
	if err := archive.Close(); err != nil {
		it.Fail("cannot finish %s: %v", path, err)
	}
	if err := gz.Close(); err != nil {
		it.Fail("cannot finish %s: %v", path, err)
	}
}

// The nonce changes the tarball's integrity on every run, so Bazel's
// repository cache cannot answer the fetch and the server sees it.
func publish(it *harness.IT, root, tarballPath, name, nonce string) string {
	writeTarball(it, filepath.Join(root, tarballPath), map[string]string{
		"package/package.json": fmt.Sprintf(`{
  "name": "%s",
  "version": "1.0.0",
  "description": "throwaway registry fixture %s",
  "main": "index.js",
  "types": "index.d.ts"
}
`, name, nonce),
		"package/index.js":           greeterJS,
		"package/index.d.ts":         greeterDTS,
		"package/BUILD/sentinel.txt": "keep the BUILD directory\n",
		"package/build/index.js":     "export const preserved = true;\n",
		"package/BUILD.bazel":        "package(default_visibility = [])\n",
		"package/WORKSPACE":          "workspace(name = \"fixture\")\n",
		"package/_pkg/sentinel.txt":  "keep the shipped staging directory\n",
		"package/_PKG0":              "keep the uppercase candidate\n",
	})
	tarball, err := os.ReadFile(filepath.Join(root, tarballPath))
	if err != nil {
		it.Fail("cannot read the tarball back: %v", err)
	}
	sum := sha512.Sum512(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	it.Pass("built %s tarball (%s)", name, integrity)
	return integrity
}

func serve(it *harness.IT, served *registry) (address string, port int) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		it.Fail("cannot listen on 127.0.0.1: %v", err)
	}
	server := &http.Server{Handler: served}
	go server.Serve(listener)
	it.OnCleanup(func() { server.Close() })
	port = listener.Addr().(*net.TCPAddr).Port
	address = fmt.Sprintf("http://127.0.0.1:%d", port)
	it.Pass("throwaway registry listening on %s", address)

	probe, err := http.Get(address + "/__probe__")
	if err != nil {
		it.Fail("unauthenticated probe failed to connect: %v", err)
	}
	probe.Body.Close()
	if probe.StatusCode != http.StatusUnauthorized {
		it.Fail("unauthenticated probe returned %d, want 401", probe.StatusCode)
	}
	it.Pass("unauthenticated request to %s is refused (401)", address)
	return address, port
}

func repoName(it *harness.IT, suffix string) string {
	external := filepath.Join(it.OutputBase, "external")
	names := it.Glob(external, "", suffix)
	if len(names) != 1 {
		it.Fail("want one *%s repository under %s, got %v", suffix, external, names)
	}
	return names[0]
}

func fetch(it *harness.IT, env string, repos ...string) {
	args := []string{"fetch", env}
	for _, repo := range repos {
		args = append(args, "--repo=@@"+repo)
	}
	it.MustBazel(args...)
}

// Bazel records each variable a fetch read, token included, beside a repo
// contents cache entry, so a repository that read one must stay out of it.
func requireLinked(it *harness.IT, suffix string, want bool) {
	name := repoName(it, suffix)
	info, err := os.Lstat(filepath.Join(it.OutputBase, "external", name))
	if err != nil {
		it.Fail("cannot stat %s: %v", name, err)
	}
	if linked := info.Mode()&fs.ModeSymlink != 0; linked != want {
		it.Fail("%s: a link into the repo contents cache is %v, want %v", name, linked, want)
	}
}

func authSetting(registry, token string) string {
	return "--repo_env=PNPM_CONFIG__AUTH=" +
		fmt.Sprintf(`{"%s":{"@lumen":{"authToken":"%s"}}}`, registry, token)
}

func lockfile(acmeIntegrity, lumenIntegrity string) string {
	return fmt.Sprintf(`lockfileVersion: '9.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      '@acme/greeter':
        specifier: 1.0.0
        version: 1.0.0
      '@lumen/greeter':
        specifier: 1.0.0
        version: 1.0.0

packages:

  '@acme/greeter@1.0.0':
    resolution: {integrity: %s}

  '@lumen/greeter@1.0.0':
    resolution: {integrity: %s}

snapshots:

  '@acme/greeter@1.0.0': {}

  '@lumen/greeter@1.0.0': {}
`, acmeIntegrity, lumenIntegrity)
}

func main() {
	harness.Run(harness.Config{
		Name:         "npm_registries",
		WorkspaceRel: "tests/integration/npm_registries/workspace",
		Renames:      map[string]string{"BUILD.bazel.tpl": "BUILD.bazel"},
	}, func(it *harness.IT) {
		nonce := fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid())
		acme := &registry{root: it.Scratch("acme"), token: acmeToken}
		lumen := &registry{root: it.Scratch("lumen"), token: lumenToken}
		acmeIntegrity := publish(it, acme.root, acmeTarball,
			"@acme/greeter", nonce)
		lumenIntegrity := publish(it, lumen.root, lumenTarball,
			"@lumen/greeter", nonce)
		acmeAddress, acmePort := serve(it, acme)
		lumenAddress, _ := serve(it, lumen)

		// The default serves nothing, so a scope neither file maps has nowhere
		// to go; @lumen is pnpm-workspace.yaml's alone, its token in no file.
		it.Write(it.Path(".npmrc"), fmt.Sprintf(`registry=%s/__default__/
@acme:registry=%s
//127.0.0.1:%d/:_authToken=${ACME_TOKEN}
`, acmeAddress, acmeAddress, acmePort))
		it.Write(it.Path("pnpm-workspace.yaml"), fmt.Sprintf(`registries:
  "@lumen": %s
`, lumenAddress))
		it.Write(it.Path("pnpm-lock.yaml"),
			lockfile(acmeIntegrity, lumenIntegrity))
		it.Pass(".npmrc, pnpm-workspace.yaml and pnpm-lock.yaml written")

		// Written here: a BUILD file under workspace/ would make the directory
		// a package of the outer workspace and glob_workspace_files would stop.
		it.Write(it.Path("src/consumer/BUILD.bazel"), consumerPackage)

		rightAcme := "--repo_env=ACME_TOKEN=" + acmeToken
		rightAuth := authSetting(lumenAddress, lumenToken)

		// Both tokens wrong in one step, before any fetch succeeds: a tarball
		// the repository cache holds is never asked of the registry again.
		wrong, err := it.BazelLog("wrong_tokens.log", "build", "--keep_going",
			"--repo_env=ACME_TOKEN=wrong-token",
			authSetting(lumenAddress, "wrong-token"), "//...")
		if err == nil {
			wrong.Dump()
			it.Fail("the build succeeded with both tokens wrong; neither " +
				"credential is reaching its registry")
		}
		if !acme.saw("401 /" + acmeTarball + " Bearer wrong-token") {
			acme.dump("acme")
			wrong.Dump()
			it.Fail("the .npmrc's registry never saw the @acme request " +
				"carrying the wrong token")
		}
		it.Pass("the wrong .npmrc _authToken is sent as a Bearer header " +
			"and rejected")
		if !lumen.saw("401 /" + lumenTarball + " Bearer wrong-token") {
			lumen.dump("lumen")
			acme.dump("acme")
			wrong.Dump()
			if m := lumenFetch.FindStringSubmatch(wrong.Text); m != nil {
				it.Fail("the @lumen tarball was fetched from %s, not from "+
					"the registry pnpm-workspace.yaml maps @lumen to (%s)",
					m[1], lumenAddress)
			}
			it.Fail("the registry pnpm-workspace.yaml maps @lumen to never " +
				"saw the request carrying the wrong PNPM_CONFIG__AUTH token")
		}
		it.Pass("the wrong PNPM_CONFIG__AUTH token is sent as a Bearer " +
			"header to pnpm-workspace.yaml's registry and rejected")

		err = it.Bazel("build", rightAcme, rightAuth, "//...", declarations)
		if err != nil {
			acme.dump("acme")
			lumen.dump("lumen")
			it.Fail("bazel build //... %s exited non-zero (both registry "+
				"fetches should succeed)", declarations)
		}
		it.Pass("bazel build //... %s through both registries", declarations)

		packages, err := filepath.Glob(filepath.Join(it.OutputBase, "external", "*", "node_modules", "@acme", "greeter", "package.json"))
		if err != nil || len(packages) != 1 {
			it.Fail("expected one fetched greeter package, got %v: %v", packages, err)
		}
		packageDir := filepath.Dir(packages[0])
		it.RequireContains(filepath.Join(packageDir, "BUILD", "sentinel.txt"), "keep the BUILD directory", "npm_import removed the BUILD directory")
		it.RequireContains(filepath.Join(packageDir, "build", "index.js"), "export const preserved = true;", "npm_import removed the build directory")
		it.RequireContains(filepath.Join(packageDir, "_pkg", "sentinel.txt"), "keep the shipped staging directory", "npm_import changed the shipped _pkg directory")
		it.RequireContains(filepath.Join(packageDir, "_PKG0"), "keep the uppercase candidate", "npm_import changed the uppercase staging candidate")
		for _, name := range []string{"BUILD.bazel", "WORKSPACE"} {
			it.RequireNoFile(filepath.Join(packageDir, name), "npm_import kept the %s boundary file", name)
		}

		if !acme.saw("200 /" + acmeTarball + " Bearer " + acmeToken) {
			acme.dump("acme")
			it.Fail(".npmrc's _authToken did not arrive as an " +
				"Authorization: Bearer header")
		}
		it.Pass(".npmrc's _authToken arrived as an Authorization: Bearer " +
			"header")

		if !lumen.saw("200 /" + lumenTarball + " Bearer " + lumenToken) {
			lumen.dump("lumen")
			it.Fail("PNPM_CONFIG__AUTH's authToken did not arrive as an " +
				"Authorization: Bearer header")
		}
		it.Pass("PNPM_CONFIG__AUTH's authToken arrived as an " +
			"Authorization: Bearer header")

		tsgo := "+tsgo_" + runtime.GOOS + "_" + runtime.GOARCH
		tsgoRepo := "rules_typescript++ts" + tsgo
		acmeSpoke, lumenSpoke := "+npm__acme_greeter__1_0_0", "+npm__lumen_greeter__1_0_0"

		// tsgo comes from registry.npmjs.org, which no .npmrc line covers, so
		// it reads no ${VAR}: only the spoke that does is kept out.
		fetch(it, rightAcme, repoName(it, acmeSpoke), tsgoRepo)
		requireLinked(it, tsgo, true)
		requireLinked(it, acmeSpoke, false)
		it.Pass("a fetch that read only the .npmrc's ${ACME_TOKEN} is not in the repo contents cache")
		fetch(it, rightAuth, repoName(it, lumenSpoke), tsgoRepo)
		requireLinked(it, lumenSpoke, false)
		requireLinked(it, tsgo, false)
		it.Pass("every fetch that read a set PNPM_CONFIG__AUTH is not in the repo contents cache")

		it.Write(it.Path(".npmrc"), fmt.Sprintf(`registry=%s/__default__/
@acme:registry=%s
//127.0.0.1:%d/:_authToken=%s
`, acmeAddress, acmeAddress, acmePort, acmeToken))
		fetch(it, "--repo_env=ACME_TOKEN=", repoName(it, acmeSpoke))
		requireLinked(it, acmeSpoke, false)
		it.Pass("a fetch with a literal .npmrc _authToken is not in the repo contents cache")

		if strings.Contains(lumen.log(), acmeToken) {
			lumen.dump("lumen")
			it.Fail("the .npmrc's token for one port was sent to the other")
		}
		it.Pass("the .npmrc's token stayed on its own port")

		served := map[string]*registry{"acme": acme, "lumen": lumen}
		for name, r := range served {
			if strings.Contains(r.log(), "__default__") {
				r.dump(name)
				it.Fail("a scope mapping was ignored: a fetch went to the " +
					"default registry")
			}
		}
		it.Pass("@acme:registry and pnpm-workspace.yaml's @lumen both " +
			"overrode the default registry")

		for _, rel := range []string{"greeting.js", "greeting.d.ts"} {
			rel = "src/consumer/" + rel
			it.RequireFile(it.Bin(rel), "expected output file not found: %s",
				rel)
			it.Pass("output file exists: %s", rel)
		}
		dts := it.Bin("src/consumer/greeting.d.ts")
		for _, export := range []string{"message", "lumenMessage"} {
			if !it.Contains(dts, export) {
				it.Dump(dts)
				it.Fail("src/consumer/greeting.d.ts does not declare the "+
					"exported %s", export)
			}
		}
		it.Pass("src/consumer/greeting.d.ts declares the exports typed from " +
			"both registries' packages")
	})
}
