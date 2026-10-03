package harness

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// An install archive names the workspace, the pnpm store and the registry by
// these, so it is the same wherever it was made and pnpm sees its own again.
const (
	archiveWorkspace = "{WORKSPACE}"
	archiveStore     = "{PNPM_STORE_DIR}"
	archiveRegistry  = "{REGISTRY}"
	defaultRegistry  = "https://registry.npmjs.org/"
	// Marks the files PackInstall rewrote: a package's own text is left alone.
	archiveRewritten = "RULESTS.rewritten"
)

var archiveClock = []struct {
	file    string
	pattern *regexp.Regexp
	fixed   string
}{
	{".modules.yaml", regexp.MustCompile(`"prunedAt": "[^"]*"`), `"prunedAt": "Thu, 01 Jan 1970 00:00:00 GMT"`},
	{".pnpm-workspace-state-v1.json", regexp.MustCompile(`"lastValidatedTimestamp": \d+`), `"lastValidatedTimestamp": 0`},
}

// ServeRegistry answers pnpm's tarball requests from dir, which holds them at
// the paths the npm registry serves them from; a miss is a 404 unless
// fallback names a registry to redirect it to.
func ServeRegistry(dir, fallback string) (url string, stop func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	files := http.FileServer(http.Dir(dir))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r.URL.Path))); err != nil && fallback != "" {
			http.Redirect(w, r, strings.TrimSuffix(fallback, "/")+r.URL.Path, http.StatusFound)
			return
		}
		files.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	return "http://" + listener.Addr().String() + "/", func() { server.Close() }, nil
}

// UseRegistry has Install fetch the tarballs under dir locally; a check that
// re-resolves the lockfile still fetches what it adds from the npm registry.
func (it *IT) UseRegistry(dir string) {
	url, stop, err := ServeRegistry(dir, defaultRegistry)
	if err != nil {
		it.Fail("cannot serve the registry tarballs: %v", err)
	}
	it.registry = url
	it.OnCleanup(stop)
}

// PackInstall moves every node_modules tree under root into archive, a tar
// with no timestamps or owners: store and registry are what the install ran
// with, recorded as Install's own.
func PackInstall(root, archive, store, registry string) error {
	var trees []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "node_modules" {
			trees = append(trees, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(trees) == 0 {
		return errors.New("no node_modules under " + root)
	}
	out, err := os.Create(archive)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(out)
	replacer := strings.NewReplacer(store, archiveStore, root, archiveWorkspace, registry, archiveRegistry)
	for _, tree := range trees {
		if err := packTree(writer, root, tree, replacer); err != nil {
			out.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	for _, tree := range trees {
		if err := os.RemoveAll(tree); err != nil {
			return err
		}
	}
	return nil
}

func packTree(writer *tar.Writer, root, tree string, replacer *strings.Replacer) error {
	var paths []string
	if err := filepath.WalkDir(tree, func(path string, _ fs.DirEntry, err error) error {
		paths = append(paths, path)
		return err
	}); err != nil {
		return err
	}
	slices.Sort(paths)
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header := &tar.Header{Name: filepath.ToSlash(rel), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		var body []byte
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("%s links to the absolute %s, which no other workspace has", rel, target)
			}
			header.Typeflag, header.Linkname, header.Mode = tar.TypeSymlink, target, 0o777
		case info.IsDir():
			header.Typeflag, header.Name, header.Mode = tar.TypeDir, header.Name+"/", 0o755
		case info.Mode().IsRegular():
			if body, err = os.ReadFile(path); err != nil {
				return err
			}
			original := body
			body = []byte(replacer.Replace(string(body)))
			for _, clock := range archiveClock {
				if info.Name() == clock.file {
					body = clock.pattern.ReplaceAll(body, []byte(clock.fixed))
				}
			}
			if !bytes.Equal(body, original) {
				header.PAXRecords = map[string]string{archiveRewritten: "1"}
			}
			header.Typeflag, header.Size, header.Mode = tar.TypeReg, int64(len(body)), 0o644
			if info.Mode()&0o111 != 0 {
				header.Mode = 0o755
			}
		default:
			return fmt.Errorf("%s is neither a file, a directory nor a symlink", rel)
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := writer.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// RestoreInstall unpacks an archive PackInstall wrote into the workspace, so
// it holds what Install would have left there.
func (it *IT) RestoreInstall(archive string) {
	in, err := os.Open(archive)
	if err != nil {
		it.Fail("cannot open the install archive: %v", err)
	}
	defer in.Close()
	registry := it.registry
	if registry == "" {
		registry = defaultRegistry
	}
	replacer := strings.NewReplacer(archiveWorkspace, it.WorkspaceDir,
		archiveStore, filepath.Join(cacheRoot(), "pnpm", "store"), archiveRegistry, registry)
	reader := tar.NewReader(in)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			it.Fail("cannot read the install archive: %v", err)
		}
		path := it.Path(filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o755)
		case tar.TypeSymlink:
			err = os.Symlink(header.Linkname, path)
		case tar.TypeReg:
			var body []byte
			if body, err = io.ReadAll(reader); err == nil {
				if header.PAXRecords[archiveRewritten] != "" {
					body = []byte(replacer.Replace(string(body)))
				}
				err = os.WriteFile(path, body, os.FileMode(header.Mode))
			}
		default:
			err = fmt.Errorf("unexpected entry type %c", header.Typeflag)
		}
		if err != nil {
			it.Fail("cannot unpack %s: %v", header.Name, err)
		}
	}
	it.RequireFile(it.Path("node_modules", ".modules.yaml"),
		"the install archive left no node_modules/.modules.yaml at the workspace root")
}
