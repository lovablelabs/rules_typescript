package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type cleanupResource struct {
	Path      string
	Recursive bool
}

type cleanupResources []cleanupResource

func (resources cleanupResources) remove() error {
	var failures []error
	for _, resource := range resources {
		remove := os.Remove
		if resource.Recursive {
			if keptForBazel(resource.Path) {
				continue
			}
			remove = os.RemoveAll
		}
		if err := remove(resource.Path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Errorf("ts_launcher: cleaning %q: %w", resource.Path, err))
		}
	}
	return errors.Join(failures...)
}

// Opt-in because a local `bazel test` keeps TEST_TMPDIR until the next run;
// a remote executor discards it with the action.
func keptForBazel(path string) bool {
	tmp := os.Getenv("TEST_TMPDIR")
	if os.Getenv("TS_TEST_SKIP_TMPDIR_CLEANUP") != "1" || tmp == "" {
		return false
	}
	tmp, err := filepath.Abs(tmp)
	if err != nil {
		return false
	}
	return strings.HasPrefix(path, tmp+string(filepath.Separator))
}

func (p *Plan) own(path string, recursive bool) error {
	resource := cleanupResource{Path: path, Recursive: recursive}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errors.Join(fmt.Errorf("ts_launcher: resolving cleanup path %q: %w", path, err), cleanupResources{resource}.remove())
	}
	resource.Path = absolute
	p.cleanup = append(p.cleanup, resource)
	return nil
}

func (p *Plan) Cleanup() {
	if err := p.cleanup.remove(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
