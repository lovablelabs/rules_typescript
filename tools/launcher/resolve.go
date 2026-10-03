package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// Resolver turns the runfiles paths in a Config into filesystem paths, and is
// the only place that knows how runfiles are laid out.
type Resolver struct {
	rf  *runfiles.Runfiles
	dir string
}

func NewResolver() (*Resolver, error) {
	return resolverForExecutable(os.Args[0])
}

func resolverForExecutable(argv0 string) (*Resolver, error) {
	// A subprocess can inherit another executable's runfiles environment.
	for _, executable := range candidateArgv0(argv0) {
		executable, err := filepath.Abs(executable)
		if err != nil {
			return nil, err
		}
		directory := executable + ".runfiles"
		for _, manifest := range []string{executable + ".runfiles_manifest", filepath.Join(directory, "MANIFEST")} {
			if !isRegular(manifest) {
				continue
			}
			return newResolver(runfiles.ManifestFile(manifest))
		}
		if st, err := os.Stat(directory); err == nil && st.IsDir() {
			return directoryResolver(directory)
		}
	}
	return newResolver()
}

func newResolver(opts ...runfiles.Option) (*Resolver, error) {
	rf, err := runfiles.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("ts_launcher: %w", err)
	}
	return &Resolver{rf: rf, dir: runfilesDir(rf.Env())}, nil
}

// directoryResolver resolves through the runfiles tree at dir, whatever the
// environment names.
func directoryResolver(dir string) (*Resolver, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Resolver{dir: abs}, nil
}

// runfilesDir returns the absolute runfiles directory, or "" when the layout is
// manifest-only. Callers must treat "" as "no tree to write into".
func runfilesDir(env []string) string {
	if runfilesEnv(env, "RUNFILES_MANIFEST_FILE") != "" {
		return ""
	}
	for _, v := range []string{runfilesEnv(env, "RUNFILES_DIR"), runfilesEnv(env, "TEST_SRCDIR")} {
		if v == "" {
			continue
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			continue
		}
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			return abs
		}
	}
	return ""
}

func runfilesEnv(env []string, name string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

// Dir is the absolute runfiles directory, or "" in manifest-only mode.
func (r *Resolver) Dir() string { return r.dir }

func (r *Resolver) Stage(root string, modules []string) (*Resolver, error) {
	manifest := runfilesEnv(r.Env(), "RUNFILES_MANIFEST_FILE")
	if manifest != "" || r.dir == "" {
		if err := stageManifest(root, manifest, func(string) bool { return true }, modules); err != nil {
			return nil, err
		}
	} else {
		sourceRoot, err := filepath.EvalSymlinks(r.dir)
		if err != nil {
			return nil, err
		}
		entries := map[string]string{}
		err = filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(sourceRoot, path)
			if err != nil {
				return err
			}
			target := path
			if entry.Type()&os.ModeSymlink != 0 {
				target, err = os.Readlink(path)
				if err != nil {
					return err
				}
				if !filepath.IsAbs(target) {
					original := filepath.Join(filepath.Dir(path), target)
					relativeTarget, err := filepath.Rel(sourceRoot, original)
					if err != nil {
						return err
					}
					if !filepath.IsLocal(relativeTarget) {
						target = original
					}
				}
			}
			entries[filepath.ToSlash(relative)] = target
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := stageEntries(root, entries, modules); err != nil {
			return nil, err
		}
	}
	return directoryResolver(root)
}

// Env returns the runfiles variables to hand to child processes.
func (r *Resolver) Env() []string {
	if r.rf == nil {
		env := []string{"RUNFILES_DIR=" + r.dir, "JAVA_RUNFILES=" + r.dir, "RUNFILES_MANIFEST_FILE=", "RUNFILES_MANIFEST_ONLY="}
		if os.Getenv("TEST_SRCDIR") != "" {
			env = append(env, "TEST_SRCDIR="+r.dir)
		}
		return env
	}
	return r.rf.Env()
}

// Path resolves one runfiles path to an absolute filesystem path.
func (r *Resolver) Path(rlocation string) (string, error) {
	if rlocation == "" {
		return "", fmt.Errorf("ts_launcher: empty runfiles path")
	}
	if r.rf == nil {
		if !fs.ValidPath(rlocation) || !filepath.IsLocal(filepath.FromSlash(rlocation)) {
			return "", fmt.Errorf("ts_launcher: non-normalized runfiles path %q", rlocation)
		}
		// Rule-generated config paths already use canonical repository names.
		return filepath.Join(r.dir, filepath.FromSlash(rlocation)), nil
	}
	p, err := r.rf.Rlocation(rlocation)
	if err != nil {
		return "", fmt.Errorf("ts_launcher: runfiles lookup failed for %q: %w", rlocation, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}
