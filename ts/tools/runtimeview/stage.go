package runtimeview

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Bindings is filled from BindingsSet, an index into the config's
// npm_binding_sets, by ResolveNpmBindings.
type NpmContext struct {
	Module       string            `json:"module"`
	Source       string            `json:"source"`
	Bindings     map[string]string `json:"bindings,omitempty"`
	BindingsSet  *int              `json:"bindings_set,omitempty"`
	PackageScope *NpmPackageScope  `json:"package_scope,omitempty"`
}

type NpmPackageScope struct {
	Manifest    string            `json:"manifest"`
	Bindings    map[string]string `json:"bindings,omitempty"`
	BindingsSet *int              `json:"bindings_set,omitempty"`
}

func ResolveNpmBindings(contexts []NpmContext, sets []map[string]string) error {
	pick := func(i *int, bindings *map[string]string) error {
		if i == nil {
			return nil
		}
		if *i < 0 || *i >= len(sets) {
			return fmt.Errorf("ts_launcher: npm bindings set %d of %d", *i, len(sets))
		}
		*bindings = sets[*i]
		return nil
	}
	for i := range contexts {
		if err := pick(contexts[i].BindingsSet, &contexts[i].Bindings); err != nil {
			return err
		}
		if scope := contexts[i].PackageScope; scope != nil {
			if err := pick(scope.BindingsSet, &scope.Bindings); err != nil {
				return err
			}
		}
	}
	return nil
}

type npmDemand struct {
	context NpmContext
	name    string
	binding string
	store   string
}

type npmLookup struct {
	anchor   string
	bindings map[string]string
}

func nativeContextPath(root, path string) (string, error) {
	if path == "." || !fs.ValidPath(path) || !filepath.IsLocal(filepath.FromSlash(path)) {
		return "", fmt.Errorf("ts_launcher: non-normalized npm context path %q", path)
	}
	return filepath.Join(root, filepath.FromSlash(path)), nil
}

func npmContextConflict(d npmDemand, destination, occupied string) error {
	return fmt.Errorf("ts_launcher: source %q npm %q from store %q for module %q: projection destination %q conflicts with %s. Remove or relocate the conflicting entry, or preserve separate runtime contexts.",
		d.context.Source, d.name, d.binding, d.context.Module, destination, occupied)
}

func PlaceNpmContexts(root string, contexts []NpmContext, modules []string) error {
	if root != "" {
		// Stores below resolve to real paths; the links must count ".." from the real root too.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		root = resolved
	}
	byDirectory := map[string]map[string]npmDemand{}
	for _, context := range contexts {
		if root == "" || !slices.Contains(modules, context.Module) {
			return fmt.Errorf("ts_launcher: npm context module %q has no admitted runtime module", context.Module)
		}
		lookups := []npmLookup{{anchor: context.Module, bindings: context.Bindings}}
		if scope := context.PackageScope; scope != nil {
			lookups = append(lookups, npmLookup{anchor: scope.Manifest, bindings: scope.Bindings})
		}
		for _, lookup := range lookups {
			if _, err := nativeContextPath(root, lookup.anchor); err != nil {
				return err
			}
			for name, store := range lookup.bindings {
				directory := filepath.ToSlash(filepath.Dir(lookup.anchor))
				destination := directory + "/node_modules/" + name
				if _, err := nativeContextPath(root, destination); err != nil {
					return err
				}
				path, err := nativeContextPath(root, store)
				if err != nil {
					return err
				}
				canonical, err := filepath.EvalSymlinks(path)
				if err != nil {
					return fmt.Errorf("ts_launcher: npm store %q for %q: %w", store, context.Module, err)
				}
				info, err := os.Stat(canonical)
				if err != nil || !info.IsDir() {
					return fmt.Errorf("ts_launcher: npm store %q for %q is not a directory", store, context.Module)
				}
				demand := npmDemand{context: context, name: name, binding: store, store: canonical}
				if byDirectory[directory] == nil {
					byDirectory[directory] = map[string]npmDemand{}
				}
				if previous, exists := byDirectory[directory][name]; exists {
					previousInfo, err := os.Stat(previous.store)
					if err != nil || !os.SameFile(previousInfo, info) {
						return npmContextConflict(demand, destination, fmt.Sprintf("source %q selecting store %q", previous.context.Source, previous.binding))
					}
				}
				byDirectory[directory][name] = demand
			}
		}
	}
	for _, directory := range slices.Sorted(maps.Keys(byDirectory)) {
		for _, name := range slices.Sorted(maps.Keys(byDirectory[directory])) {
			demand := byDirectory[directory][name]
			destination := directory + "/node_modules/" + name
			occupied := ""
			parts := strings.Split(destination, "/")
			for depth := 1; depth <= len(parts); depth++ {
				relative := strings.Join(parts[:depth], "/")
				info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
				if err == nil {
					if depth == len(parts) || !info.IsDir() {
						occupied = relative
						break
					}
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			if occupied == "" {
				if err := relativeLink(filepath.Join(root, filepath.FromSlash(destination)), demand.store); err != nil {
					return err
				}
				continue
			}
			matched := false
			parts = strings.Split(directory, "/")
			for depth := len(parts); depth > 0; depth-- {
				if parts[depth-1] == "node_modules" {
					continue
				}
				candidate := filepath.Join(root, filepath.FromSlash(strings.Join(parts[:depth], "/")+"/node_modules/"+name))
				if _, err := os.Lstat(candidate); err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return err
				}
				actual, actualErr := os.Stat(candidate)
				expected, expectedErr := os.Stat(demand.store)
				matched = actualErr == nil && expectedErr == nil && os.SameFile(actual, expected)
				break
			}
			if !matched {
				return npmContextConflict(demand, destination, fmt.Sprintf("runtime entry %q", occupied))
			}
		}
	}
	return nil
}

func StageManifest(root, manifest string, include func(string) bool, modules []string) error {
	if manifest == "" {
		return errors.New("ts_launcher: no runfiles directory and no " +
			"RUNFILES_MANIFEST_FILE")
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	entries := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		rlocation, target, ok := ManifestEntry(line)
		if !ok || !include(rlocation) {
			continue
		}
		if rlocation == "." || !fs.ValidPath(rlocation) || !filepath.IsLocal(filepath.FromSlash(rlocation)) {
			return fmt.Errorf("ts_launcher: non-normalized runfiles path %q", rlocation)
		}
		entries[rlocation] = target
	}
	return Stage(root, entries, modules)
}

func Stage(root string, entries map[string]string, modules []string) error {
	paths := slices.SortedFunc(maps.Keys(entries), func(a, b string) int { return cmp.Compare(a+"/", b+"/") })
	parent := ""
	for _, rlocation := range paths {
		// Never write an exact child entry through a directory alias.
		if parent != "" && strings.HasPrefix(rlocation, parent) {
			continue
		}
		link := filepath.Join(root, filepath.FromSlash(rlocation))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		if target := entries[rlocation]; target != "" {
			if err := os.Symlink(filepath.FromSlash(target), link); err != nil {
				return err
			}
		} else if err := os.WriteFile(link, nil, 0o644); err != nil {
			return err
		}
		parent = rlocation + "/"
	}
	materialized, err := validateEntries(root, entries, modules, paths)
	if err != nil {
		return err
	}
	if err := linkCanonicalModules(root, entries, paths, materialized); err != nil {
		return err
	}
	for _, rlocation := range paths {
		if materialized[rlocation] {
			if err := materializeModule(filepath.Join(root, filepath.FromSlash(rlocation))); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEntries(root string, entries map[string]string, modules, paths []string) (map[string]bool, error) {
	targetPath := func(name string) string {
		target := filepath.FromSlash(entries[name])
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, filepath.Dir(filepath.FromSlash(name)), target)
		}
		return filepath.Clean(target)
	}
	materialized := map[string]bool{}
	canonical := map[string]string{}
	for _, module := range modules {
		if module == "." || !fs.ValidPath(module) || !filepath.IsLocal(filepath.FromSlash(module)) {
			return nil, fmt.Errorf("ts_launcher: non-normalized module path %q", module)
		}
		if _, exists := entries[module]; !exists {
			return nil, fmt.Errorf("ts_launcher: module %q has no individual runfiles entry; expose its runfiles individually", module)
		}
		if entries[module] != "" {
			target := targetPath(module)
			if previous, exists := canonical[target]; exists && previous != module {
				return nil, fmt.Errorf("ts_launcher: module artifact %q is published at multiple paths %q and %q; expose one canonical module path", target, previous, module)
			}
			canonical[target] = module
		}
		materialized[module] = true
	}
	parent := ""
	for _, name := range paths {
		if parent == "" || !strings.HasPrefix(name, parent+"/") {
			parent = name
			continue
		}
		target := filepath.Join(targetPath(parent), filepath.FromSlash(strings.TrimPrefix(name, parent+"/")))
		actual, actualErr := os.Stat(target)
		expected, expectedErr := os.Stat(targetPath(name))
		if entries[parent] == "" || entries[name] == "" || actualErr != nil || expectedErr != nil || !os.SameFile(actual, expected) {
			return nil, fmt.Errorf("ts_launcher: runfiles manifest entry %q conflicts with directory entry %q; remove the overlapping runfiles mappings", name, parent)
		}
		if materialized[name] {
			return nil, fmt.Errorf("ts_launcher: cannot stage module %q beneath directory entry %q; expose its runfiles individually", name, parent)
		}
	}
	return materialized, nil
}

func linkCanonicalModules(root string, entries map[string]string, paths []string, modules map[string]bool) error {
	canonical := map[string][]string{}
	for module := range modules {
		at := filepath.Join(root, filepath.FromSlash(module))
		target := filepath.FromSlash(entries[module])
		if target == "" {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(at), target)
		}
		canonical[filepath.Clean(target)] = append(canonical[filepath.Clean(target)], module)
		canonical[at] = append(canonical[at], module)
	}
	parent := ""
	for _, name := range paths {
		if parent != "" && strings.HasPrefix(name, parent) {
			continue
		}
		parent = name + "/"
		if modules[name] || entries[name] == "" {
			continue
		}
		at := filepath.Join(root, filepath.FromSlash(name))
		target := filepath.FromSlash(entries[name])
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(at), target)
		}
		candidates := canonical[filepath.Clean(target)]
		if len(candidates) == 0 {
			continue
		}
		if len(candidates) != 1 {
			return fmt.Errorf("ts_launcher: alias %q reaches module artifact %q published at multiple paths %q; expose one canonical module path", name, target, candidates)
		}
		destination := filepath.Join(root, filepath.FromSlash(candidates[0]))
		relative, err := filepath.Rel(filepath.Dir(at), destination)
		if err != nil {
			return err
		}
		if err := os.Remove(at); err != nil {
			return err
		}
		if err := os.Symlink(relative, at); err != nil {
			return err
		}
	}
	return nil
}

func materializeModule(path string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ts_module-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if err := temporary.Close(); err != nil {
		return err
	}
	defer os.Remove(name)
	if err := os.Remove(name); err != nil {
		return err
	}
	if err := copyRegular(path, name); err != nil {
		return err
	}
	// Replacing our link must never write into the selected source artifact.
	return os.Rename(name, path)
}

// ManifestEntry reads one runfiles manifest line, "<rlocation> <target>"; a
// line starting with a space escapes both as \s, \n and \b.
func ManifestEntry(line string) (rlocation, target string, ok bool) {
	escaped := strings.HasPrefix(line, " ")
	rlocation, target, ok = strings.Cut(strings.TrimPrefix(line, " "), " ")
	if !ok || rlocation == "" {
		return "", "", false
	}
	if escaped {
		unescape := strings.NewReplacer(`\s`, " ", `\n`, "\n", `\b`, `\`)
		rlocation, target = unescape.Replace(rlocation), unescape.Replace(target)
	}
	return rlocation, target, true
}
