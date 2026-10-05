package runtimeview

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Input struct {
	Path   string `json:"path"`
	Output string `json:"output"`
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	// Form is a placed input's declared shape; sandboxes stage regular inputs as links.
	Form string `json:"form,omitempty"`
}

type PackageLink struct {
	Name        string `json:"name"`
	PackageJSON string `json:"package_json"`
}

type Spec struct {
	Root         string              `json:"root"`
	Inputs       []Input             `json:"inputs"`
	Entries      map[string]string   `json:"entries"`
	Modules      []string            `json:"modules"`
	NpmContexts  []NpmContext        `json:"npm_contexts"`
	NpmBindings  []map[string]string `json:"npm_binding_sets"`
	Links        map[string]string   `json:"links"`
	OptionalDeps []PackageLink       `json:"optional_deps"`
}

func Build(spec Spec) error {
	root, err := resolvedPath(spec.Root)
	if err != nil {
		return err
	}
	inputs := map[string]Input{}
	for _, input := range spec.Inputs {
		input.Path, err = filepath.Abs(input.Path)
		if err != nil {
			return err
		}
		if input.Output != "" {
			input.Output, err = resolvedPath(input.Output)
			if err != nil {
				return err
			}
		} else if input.Kind != "alias" && input.Kind != "placed" {
			return fmt.Errorf("runtime view: %s input %q has no declared output", input.Kind, input.Path)
		}
		if input.Target != "" {
			input.Target, err = resolvedPath(input.Target)
			if err != nil {
				return err
			}
		}
		if _, exists := inputs[input.Path]; exists {
			return fmt.Errorf("runtime view: repeated input authority %q", input.Path)
		}
		inputs[input.Path] = input
	}
	entries := map[string]string{}
	for name, source := range spec.Entries {
		if _, err := nativeContextPath(root, name); err != nil {
			return err
		}
		if source != "" {
			source, err = filepath.Abs(source)
			if err != nil {
				return err
			}
			if _, exists := inputs[source]; !exists {
				return fmt.Errorf("runtime view: %q has no declared input authority", name)
			}
		}
		entries[name] = source
	}
	paths := slices.SortedFunc(maps.Keys(entries), func(a, b string) int { return strings.Compare(a+"/", b+"/") })
	modules, err := validateEntries(root, entries, spec.Modules, paths)
	if err != nil {
		return err
	}
	parent := ""
	for _, name := range paths {
		if parent != "" && strings.HasPrefix(name, parent+"/") {
			delete(entries, name)
		} else {
			parent = name
		}
	}
	for _, input := range inputs {
		switch input.Kind {
		case "file":
			err = copyRegular(input.Path, input.Output)
		case "directory":
			err = copyDirectory(input.Path, input.Output)
		case "placed":
			if input.Target == "" {
				err = fmt.Errorf("placed input has no view path")
			}
		case "alias":
			if input.Output != "" {
				err = relativeLink(input.Output, input.Target)
			} else if input.Target == "" {
				err = fmt.Errorf("alias has no authority")
			}
		case "symlink":
			var target string
			target, err = os.Readlink(input.Path)
			if err == nil {
				resolved := target
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(filepath.Dir(input.Path), resolved)
				}
				if mapped := mappedInput(filepath.Clean(resolved), inputs); mapped != "" {
					err = relativeLink(input.Output, mapped)
				} else {
					err = makeLink(input.Output, target)
				}
			}
		default:
			return fmt.Errorf("runtime view: unknown declared input kind %q for %q", input.Kind, input.Path)
		}
		if err != nil {
			return fmt.Errorf("runtime view: materializing %q: %w", input.Path, err)
		}
	}
	staged := map[string]string{}
	placements := map[string]string{}
	for name, source := range entries {
		switch {
		case source != "" && inputs[source].Kind == "placed":
			placements[name] = source
		case source == "", modules[name]:
			staged[name] = source
		case inputs[source].Kind == "symlink":
			// A declared relative link keeps its link text, as in a runfiles tree.
			target, err := os.Readlink(source)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				target, err = filepath.Rel(filepath.Dir(filepath.Join(root, filepath.FromSlash(name))), inputs[source].Output)
				if err != nil {
					return err
				}
			}
			staged[name] = target
		default:
			relative, err := filepath.Rel(filepath.Dir(filepath.Join(root, filepath.FromSlash(name))), inputs[source].authority())
			if err != nil {
				return err
			}
			staged[name] = relative
		}
	}
	if err := Stage(root, staged, spec.Modules); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(placements)) {
		if err := place(filepath.Join(root, filepath.FromSlash(name)), placements[name], inputs); err != nil {
			return fmt.Errorf("runtime view: materializing %q: %w", placements[name], err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Links)) {
		output, err := nativeContextPath(root, name)
		if err != nil {
			return err
		}
		target, err := nativeContextPath(root, spec.Links[name])
		if err != nil {
			return err
		}
		if err := relativeLink(output, target); err != nil {
			return err
		}
	}
	if err := ResolveNpmBindings(spec.NpmContexts, spec.NpmBindings); err != nil {
		return err
	}
	if err := PlaceNpmContexts(root, spec.NpmContexts, spec.Modules); err != nil {
		return err
	}
	for _, dep := range spec.OptionalDeps {
		manifest, err := nativeContextPath(root, dep.PackageJSON)
		if err != nil {
			return err
		}
		manifest, err = filepath.EvalSymlinks(manifest)
		if err != nil {
			return err
		}
		output, err := nativeContextPath(filepath.Join(root, "..", "optional", "node_modules"), dep.Name)
		if err != nil {
			return err
		}
		if err := relativeLink(output, filepath.Dir(manifest)); err != nil {
			return err
		}
	}
	return nil
}

// place materializes an input at its one view path: a copy of a file or
// directory, or a link keeping a relative link's text.
func place(destination, source string, inputs map[string]Input) error {
	switch inputs[source].Form {
	case "symlink":
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			if mapped := mappedInput(filepath.Clean(target), inputs); mapped != "" {
				return relativeLink(destination, mapped)
			}
		}
		return makeLink(destination, target)
	case "directory":
		return copyDirectory(source, destination)
	case "file":
		return copyRegular(source, destination)
	default:
		return fmt.Errorf("unknown placed form %q", inputs[source].Form)
	}
}

// resolvedPath resolves the symlinks of path's existing ancestors, so relative
// links between view outputs and resolved stores count ".." in one tree.
func resolvedPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", err
		}
		missing = filepath.Join(filepath.Base(path), missing)
		path = parent
	}
}

// authority is where the view reaches an input: its declared output, or for an
// output-less alias the target it resolves to.
func (input Input) authority() string {
	if input.Output == "" {
		return input.Target
	}
	return input.Output
}

func mappedInput(path string, inputs map[string]Input) string {
	if input, exists := inputs[path]; exists {
		return input.authority()
	}
	for parent := filepath.Dir(path); parent != "." && parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if input, exists := inputs[parent]; exists && (input.Kind == "directory" || input.Kind == "placed" && input.Form == "directory") {
			relative, _ := filepath.Rel(parent, path)
			return filepath.Join(input.authority(), relative)
		}
	}
	return ""
}

func relativeLink(output, target string) error {
	if target == "" {
		return fmt.Errorf("runtime view: alias %q has no authority", output)
	}
	relative, err := filepath.Rel(filepath.Dir(output), target)
	if err != nil {
		return err
	}
	return makeLink(output, relative)
}

func makeLink(output, target string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, output)
}

func copyRegular(source, output string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("runtime view: %q is not a regular File", source)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	modeErr := out.Chmod(info.Mode().Perm())
	closeErr := out.Close()
	return errors.Join(copyErr, modeErr, closeErr)
}

func copyDirectory(source, output string) error {
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(output, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		return copyRegular(path, destination)
	})
}
