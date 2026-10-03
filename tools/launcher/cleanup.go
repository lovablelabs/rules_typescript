package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
			remove = os.RemoveAll
		}
		if err := remove(resource.Path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Errorf("ts_launcher: cleaning %q: %w", resource.Path, err))
		}
	}
	return errors.Join(failures...)
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
