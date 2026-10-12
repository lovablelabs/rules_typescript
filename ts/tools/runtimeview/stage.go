package runtimeview

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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

type npmStore struct {
	canonical string
	info      os.FileInfo
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

// PlaceNpmContexts links every demanded <directory>/node_modules/<name> that
// is free, the contract of the build-time native view.
func PlaceNpmContexts(root string, contexts []NpmContext, modules []string) error {
	return placeNpmContexts(root, contexts, modules, false)
}

// PlaceNpmContextsSkippingResolved omits a link when node's walk-up from its
// directory already reaches the same store, which changes no resolution.
func PlaceNpmContextsSkippingResolved(root string, contexts []NpmContext, modules []string) error {
	return placeNpmContexts(root, contexts, modules, true)
}

func placeNpmContexts(root string, contexts []NpmContext, modules []string, skipResolved bool) error {
	if root != "" {
		// Stores below resolve to real paths; the links must count ".." from the real root too.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		root = resolved
	}
	byDirectory := map[string]map[string]npmDemand{}
	storeCache := map[string]npmStore{}
	resolveStore := func(store, module string) (npmStore, error) {
		if resolved, ok := storeCache[store]; ok {
			return resolved, nil
		}
		path, err := nativeContextPath(root, store)
		if err != nil {
			return npmStore{}, err
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return npmStore{}, fmt.Errorf("ts_launcher: npm store %q for %q: %w", store, module, err)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			return npmStore{}, fmt.Errorf("ts_launcher: npm store %q for %q is not a directory", store, module)
		}
		resolved := npmStore{canonical: canonical, info: info}
		storeCache[store] = resolved
		return resolved, nil
	}
	moduleSet := make(map[string]struct{}, len(modules))
	for _, module := range modules {
		moduleSet[module] = struct{}{}
	}
	// ResolveNpmBindings hands every context of one npm_binding_sets entry the
	// same map, so a repeated (directory, map) pair would rewrite the same demands.
	type lookupKey struct {
		directory string
		bindings  uintptr
	}
	seen := map[lookupKey]struct{}{}
	validNames := map[string]bool{}
	for _, context := range contexts {
		if _, admitted := moduleSet[context.Module]; root == "" || !admitted {
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
			if len(lookup.bindings) == 0 {
				continue
			}
			directory := filepath.ToSlash(filepath.Dir(lookup.anchor))
			key := lookupKey{directory: directory, bindings: reflect.ValueOf(lookup.bindings).Pointer()}
			if _, done := seen[key]; done {
				continue
			}
			seen[key] = struct{}{}
			placed := byDirectory[directory]
			if placed == nil {
				placed = map[string]npmDemand{}
				byDirectory[directory] = placed
			}
			for name, store := range lookup.bindings {
				destination := directory + "/node_modules/" + name
				// The anchor check covers directory unless it is ".", so a valid name stays valid in any other directory.
				if directory == "." || !validNames[name] {
					if _, err := nativeContextPath(root, destination); err != nil {
						return err
					}
					validNames[name] = true
				}
				resolved, err := resolveStore(store, context.Module)
				if err != nil {
					return err
				}
				demand := npmDemand{context: context, name: name, binding: store, store: resolved.canonical}
				if previous, exists := placed[name]; exists && previous.binding != store {
					if previous.store != resolved.canonical && !os.SameFile(storeCache[previous.binding].info, resolved.info) {
						return npmContextConflict(demand, destination, fmt.Sprintf("source %q selecting store %q", previous.context.Source, previous.binding))
					}
				}
				placed[name] = demand
			}
		}
	}
	type pathState struct {
		exists bool
		isDir  bool
	}
	lstatCache := map[string]pathState{}
	lstat := func(relative string) (pathState, error) {
		if state, ok := lstatCache[relative]; ok {
			return state, nil
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
		state := pathState{}
		if err == nil {
			state = pathState{exists: true, isDir: info.IsDir()}
		} else if !os.IsNotExist(err) {
			return state, err
		}
		lstatCache[relative] = state
		return state, nil
	}
	statCache := map[string]os.FileInfo{}
	selects := func(candidate string, expected os.FileInfo) bool {
		actual, ok := statCache[candidate]
		if !ok {
			// A failed Stat (a dangling link) caches nil: it never selects a store.
			actual, _ = os.Stat(filepath.Join(root, filepath.FromSlash(candidate)))
			statCache[candidate] = actual
		}
		return actual != nil && os.SameFile(actual, expected)
	}
	// nearestSelects reports whether the nearest <ancestor>/node_modules/<name>
	// on node's walk-up from directory is the expected store.
	nearestSelects := func(directory, name string, expected os.FileInfo) (bool, error) {
		parts := strings.Split(directory, "/")
		for depth := len(parts); depth > 0; depth-- {
			if parts[depth-1] == "node_modules" {
				continue
			}
			candidate := strings.Join(parts[:depth], "/") + "/node_modules/" + name
			state, err := lstat(candidate)
			if err != nil {
				return false, err
			}
			if !state.exists {
				continue
			}
			return selects(candidate, expected), nil
		}
		return false, nil
	}
	type nodeModules struct {
		names map[string]bool
		// shadows holds each <base> a <base>.js, .json or .node entry
		// shadows, which CJS tries before the <base> directory.
		shadows map[string]bool
	}
	listings := map[string]nodeModules{}
	listing := func(relative string) (nodeModules, error) {
		if listed, ok := listings[relative]; ok {
			return listed, nil
		}
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil && !os.IsNotExist(err) {
			return nodeModules{}, err
		}
		listed := nodeModules{names: make(map[string]bool, len(entries)), shadows: map[string]bool{}}
		for _, entry := range entries {
			listed.names[entry.Name()] = true
			for _, extension := range []string{".js", ".json", ".node"} {
				if base, ok := strings.CutSuffix(entry.Name(), extension); ok {
					listed.shadows[base] = true
				}
			}
		}
		listings[relative] = listed
		return listed, nil
	}
	type walkStop struct {
		directory string
		listed    nodeModules
		err       error
	}
	type walkKey struct{ directory, scope string }
	walkStops := map[walkKey]*walkStop{}
	// nextStop is the nearest directory at or above directory whose
	// node_modules<scope> lists anything, or nil: an empty one never decides a walk.
	var nextStop func(directory, scope string) *walkStop
	nextStop = func(directory, scope string) *walkStop {
		key := walkKey{directory, scope}
		if stop, ok := walkStops[key]; ok {
			return stop
		}
		listed, err := listing(directory + "/node_modules" + scope)
		stop := &walkStop{directory: directory, listed: listed, err: err}
		if err == nil && len(listed.names) == 0 {
			stop = nil
			if i := strings.LastIndex(directory, "/"); i >= 0 {
				stop = nextStop(directory[:i], scope)
			}
		}
		walkStops[key] = stop
		return stop
	}
	// resolvedAbove reports whether node's CJS and ESM walks and vite's all
	// reach the expected store for name from directory without a link there.
	resolvedAbove := func(directory, name string, expected os.FileInfo) (bool, error) {
		// Only CJS skips node_modules segments on its walk, so the walks differ below one.
		if strings.Contains("/"+directory+"/", "/node_modules/") {
			return false, nil
		}
		scope, base := "", name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			scope, base = "/"+name[:i], name[i+1:]
		}
		for stop := nextStop(directory, scope); stop != nil; {
			if stop.err != nil {
				return false, stop.err
			}
			if stop.listed.shadows[base] {
				return false, nil
			}
			if stop.listed.names[base] {
				return selects(stop.directory+"/node_modules"+scope+"/"+base, expected), nil
			}
			i := strings.LastIndex(stop.directory, "/")
			if i < 0 {
				break
			}
			stop = nextStop(stop.directory[:i], scope)
		}
		return false, nil
	}
	// Sorted order settles every ancestor directory before its descendants, so
	// a walk-up sees each link this pass places above it.
	for _, directory := range slices.Sorted(maps.Keys(byDirectory)) {
		for _, name := range slices.Sorted(maps.Keys(byDirectory[directory])) {
			demand := byDirectory[directory][name]
			expected := storeCache[demand.binding].info
			if skipResolved {
				if matched, err := resolvedAbove(directory, name, expected); err == nil && matched {
					continue
				}
			}
			destination := directory + "/node_modules/" + name
			occupied := ""
			parts := strings.Split(destination, "/")
			for depth := 1; depth <= len(parts); depth++ {
				relative := strings.Join(parts[:depth], "/")
				state, err := lstat(relative)
				if err != nil {
					return err
				}
				if state.exists && (depth == len(parts) || !state.isDir) {
					occupied = relative
					break
				}
			}
			if occupied == "" {
				if err := relativeLink(filepath.Join(root, filepath.FromSlash(destination)), demand.store); err != nil {
					return err
				}
				// A new link can surface through a directory alias at another cached path. Fresh
				// maps, since clear() costs a map's peak capacity on every placement.
				lstatCache = map[string]pathState{}
				statCache = map[string]os.FileInfo{}
				listings = map[string]nodeModules{}
				walkStops = map[walkKey]*walkStop{}
				continue
			}
			matched, err := nearestSelects(directory, name, expected)
			if err != nil {
				return err
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
	fresh := isEmptyDir(root)
	// A module with an absolute target is copied straight to its path instead
	// of being linked and then replaced.
	direct := map[string]bool{}
	if !resolvesThroughTree(entries, paths) {
		for _, module := range modules {
			direct[module] = filepath.IsAbs(filepath.FromSlash(entries[module]))
		}
	}
	created := map[string]bool{}
	parent := ""
	for _, rlocation := range paths {
		// Never write an exact child entry through a directory alias.
		if parent != "" && strings.HasPrefix(rlocation, parent) {
			continue
		}
		link := filepath.Join(root, filepath.FromSlash(rlocation))
		if dir := filepath.Dir(link); !created[dir] {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			created[dir] = true
		}
		if !fresh {
			if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		parent = rlocation + "/"
		if direct[rlocation] {
			continue
		}
		if target := entries[rlocation]; target != "" {
			if err := os.Symlink(filepath.FromSlash(target), link); err != nil {
				return err
			}
		} else if err := os.WriteFile(link, nil, 0o644); err != nil {
			return err
		}
	}
	materialized, err := validateEntries(root, entries, modules, paths)
	if err != nil {
		return err
	}
	if err := linkCanonicalModules(root, entries, paths, materialized); err != nil {
		return err
	}
	// A linked module's relative target can reach another module's path, so
	// every directly copied module is in place before the linked ones.
	for _, rlocation := range paths {
		if materialized[rlocation] && direct[rlocation] {
			if err := copyRegular(filepath.FromSlash(entries[rlocation]), filepath.Join(root, filepath.FromSlash(rlocation))); err != nil {
				return err
			}
		}
	}
	for _, rlocation := range paths {
		if materialized[rlocation] && !direct[rlocation] && entries[rlocation] != "" {
			if err := materializeModule(filepath.Join(root, filepath.FromSlash(rlocation))); err != nil {
				return err
			}
		}
	}
	return nil
}

func isEmptyDir(path string) bool {
	dir, err := os.Open(path)
	if err != nil {
		return os.IsNotExist(err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1)
	return len(names) == 0 && err == io.EOF
}

// resolvesThroughTree reports whether validateEntries stats a path through
// the staged tree: an entry beneath a directory entry, either with a relative
// target.
func resolvesThroughTree(entries map[string]string, paths []string) bool {
	relative := func(name string) bool {
		target := filepath.FromSlash(entries[name])
		return target != "" && !filepath.IsAbs(target)
	}
	parent := ""
	for _, name := range paths {
		if parent == "" || !strings.HasPrefix(name, parent+"/") {
			parent = name
			continue
		}
		if relative(parent) || relative(name) {
			return true
		}
	}
	return false
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
