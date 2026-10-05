package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

type emitConfig struct {
	options, tsconfig, scratch, outDir string
	oxc, tsgo                          string
	roots, sources, importers          stringList
	inherited                          stringList
	overlays, manifests, runtimeScopes stringList
	sourceMap, declarations, esModules bool
	declarationsOnly, declarationMap   bool
	checkers                           int
	srcs                               []string
}

func runEmit(args []string) error {
	var e emitConfig
	flags := flag.NewFlagSet("emit", flag.ExitOnError)
	flags.StringVar(&e.options, "options", "",
		"the options file the tsconfig step wrote")
	flags.StringVar(&e.tsconfig, "tsconfig", "",
		"the tsconfig the tsconfig step wrote")
	flags.Var(&e.sources, "source",
		"a declared compiler input linked at its logical source path (repeatable)")
	flags.Var(&e.importers, "node_modules",
		"an importer's node_modules directory, nearest first (repeatable)")
	flags.Var(&e.inherited, "inherit_node_modules",
		"an importer node_modules directory outside the chain (repeatable)")
	flags.Var(&e.overlays, "overlay",
		"the output directory of a dep whose package is at or above this "+
			"one's; its declarations, manifest and data are laid over that "+
			"package's sources, never its JavaScript (repeatable)")
	flags.Var(&e.manifests, "manifest",
		"such a dep's package.json as built, laid at its package's "+
			"package.json over the src (repeatable)")
	flags.Var(&e.runtimeScopes, "runtime_scope", "a package scope and its declared runtime targets as JSON (repeatable)")
	flags.StringVar(&e.scratch, "scratch", "",
		"where the program root and tsgo's outDir go, removed after")
	flags.StringVar(&e.outDir, "out_dir", "",
		"the target's output directory, where every src's emit lands")
	flags.StringVar(&e.oxc, "oxc", "", "the oxc-bazel binary")
	flags.StringVar(&e.tsgo, "tsgo", "", "the tsgo binary")
	flags.Var(&e.roots, "root",
		"a directory the srcs hang off, exec-root relative (repeatable)")
	flags.BoolVar(&e.sourceMap, "source_map", false,
		"emit a .js.map beside every .js")
	flags.BoolVar(&e.declarations, "declarations", false,
		"emit a .d.ts beside every .js, with isolated declarations")
	flags.BoolVar(&e.declarationsOnly, "declarations_only", false, "emit checked declarations from the complete program")
	flags.BoolVar(&e.declarationMap, "declaration_map", false, "emit declaration maps")
	flags.IntVar(&e.checkers, "checkers", 0, "declaration checker threads")
	flags.BoolVar(&e.esModules, "es_modules", false,
		"emit ES modules whatever the options' module: oxc's transform")
	if err := flags.Parse(args); err != nil {
		return err
	}
	e.srcs = flags.Args()
	if e.options == "" {
		return errors.New("emit needs -options")
	}
	o, err := readOptions(e.options)
	if err != nil {
		return err
	}
	oxcEmits := !e.declarationsOnly && (e.esModules || tsconfig.OxcEmits(o.Module))
	required := []struct{ name, value string }{
		{"-out_dir", e.outDir},
	}
	if oxcEmits {
		required = append(required, struct{ name, value string }{"-oxc", e.oxc})
	}
	if !oxcEmits {
		required = append(required, []struct{ name, value string }{
			{"-tsconfig", e.tsconfig}, {"-scratch", e.scratch},
			{"-tsgo", e.tsgo},
		}...)
	}
	for _, required := range required {
		if required.value == "" {
			return fmt.Errorf("emit needs %s", required.name)
		}
	}
	if len(e.roots) == 0 || len(e.srcs) == 0 {
		return errors.New("emit needs a -root and a src")
	}

	groups, err := e.groupByRoot()
	if err != nil {
		return err
	}
	if err := validateRuntimeScopes(e.runtimeScopes); err != nil {
		return err
	}
	if oxcEmits {
		return e.oxcEmit(groups, o)
	}
	return e.tsgoEmit(groups, o)
}

func readOptions(name string) (oxcOptions, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return oxcOptions{}, err
	}
	var o oxcOptions
	if err := json.Unmarshal(data, &o); err != nil {
		return oxcOptions{}, fmt.Errorf("%s: %w", name, err)
	}
	return o, nil
}

// groupByRoot files each src under the longest root that contains it.
func (e *emitConfig) groupByRoot() (map[string][]string, error) {
	groups := map[string][]string{}
	for _, src := range e.srcs {
		best, found := "", false
		for _, root := range e.roots {
			if (root == "" || strings.HasPrefix(src, root+"/")) &&
				(!found || len(root) > len(best)) {
				best, found = root, true
			}
		}
		if !found {
			return nil, fmt.Errorf("%s hangs off none of the roots %q", src, e.roots)
		}
		groups[best] = append(groups[best], src)
	}
	return groups, nil
}

func sortedRoots(groups map[string][]string) []string {
	roots := make([]string, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

func (e *emitConfig) oxcEmit(groups map[string][]string, o oxcOptions) error {
	for _, root := range sortedRoots(groups) {
		cmdline := append([]string{e.oxc, "--files"}, groups[root]...)
		cmdline = append(cmdline, "--out-dir", e.outDir)
		if root != "" {
			cmdline = append(cmdline, "--strip-dir-prefix", root)
		}
		if e.sourceMap {
			cmdline = append(cmdline, "--source-map")
		}
		if e.declarations {
			cmdline = append(cmdline, "--declaration", "--isolated-declarations")
		}
		if err := runTool(append(cmdline, o.flags()...)); err != nil {
			return err
		}
	}
	return nil
}

func (e *emitConfig) tsgoEmit(groups map[string][]string, o oxcOptions) error {
	roots := sortedRoots(groups)
	root := compilerPath(roots[0])
	for _, next := range roots[1:] {
		next = compilerPath(next)
		for root != "" && next != root && !strings.HasPrefix(next, root+"/") {
			root = path.Dir(root)
			if root == "." {
				root = ""
			}
		}
	}
	programRoot := filepath.Join(e.scratch, "root")
	scratchOut := filepath.Join(e.scratch, "out")
	err := layOutProgramRoot(
		programRoot, e.sources, e.importers, e.inherited, e.overlays, e.manifests,
	)
	if err != nil {
		return err
	}
	defer os.RemoveAll(e.scratch)

	tsgo, err := filepath.Abs(e.tsgo)
	if err != nil {
		return err
	}
	cmdline := []string{tsgo, "--project", e.tsconfig}
	if !e.declarationsOnly {
		cmdline = append(cmdline, "--noCheck")
	}
	cmdline = append(cmdline,
		"--noEmit", "false", "--emitDeclarationOnly", fmt.Sprint(e.declarationsOnly),
		"--declaration", fmt.Sprint(e.declarations || e.declarationsOnly),
		"--declarationMap", fmt.Sprint(e.declarationMap),
		"--sourceMap", fmt.Sprint(e.sourceMap),
		"--inlineSources", fmt.Sprint(e.sourceMap),
		"--outDir", scratchOut, "--rootDir", rootDirFlag(root),
	)
	if e.declarationsOnly {
		cmdline = append(cmdline, "--noEmitOnError")
		if e.checkers > 0 {
			cmdline = append(cmdline, "--checkers", fmt.Sprint(e.checkers))
		}
	}
	if err := runToolIn(programRoot, os.Stdout, cmdline); err != nil {
		return err
	}
	for _, inputRoot := range roots {
		for _, src := range groups[inputRoot] {
			fromOutputs := e.outputsOf(compilerPath(src), root, o)
			for i, out := range e.outputsOf(src, inputRoot, o) {
				from, to := filepath.Join(scratchOut, fromOutputs[i]), filepath.Join(e.outDir, out)
				var err error
				if strings.HasSuffix(out, ".map") {
					err = moveMap(from, to, src, e.declarationsOnly)
				} else {
					err = moveFile(from, to)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func rootDirFlag(root string) string {
	if root == "" {
		return "."
	}
	return root
}

func (e *emitConfig) outputsOf(src, root string, o oxcOptions) []string {
	rel := src
	if root != "" {
		rel = strings.TrimPrefix(src, root+"/")
	}
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	if e.declarationsOnly {
		declaration := ".d.ts"
		switch ext {
		case ".mjs", ".mts":
			declaration = ".d.mts"
		case ".cjs", ".cts":
			declaration = ".d.cts"
		}
		out := []string{stem + declaration}
		if e.declarationMap {
			out = append(out, stem+declaration+".map")
		}
		return out
	}
	js := ".js"
	if ext == ".tsx" && strings.EqualFold(o.Jsx, "preserve") {
		js = ".jsx"
	}
	out := []string{stem + js}
	if e.sourceMap {
		out = append(out, stem+js+".map")
	}
	if e.declarations {
		out = append(out, stem+".d.ts")
	}
	return out
}

func moveFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("tsgo emitted no %s: %w", filepath.Base(to), err)
	}
	return nil
}

type sourceMap struct {
	Version        int      `json:"version"`
	File           string   `json:"file"`
	SourceRoot     string   `json:"sourceRoot"`
	Sources        []string `json:"sources"`
	Names          []string `json:"names"`
	Mappings       string   `json:"mappings"`
	SourcesContent []string `json:"sourcesContent"`
}

// Declaration maps use URL-relative sources; JS maps share oxc's exec-root convention.
func moveMap(from, to, source string, declaration bool) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("tsgo emitted no %s: %w", filepath.Base(to), err)
	}
	var m sourceMap
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	if err := restoreMapSource(&m, from, source); err != nil {
		return err
	}
	if declaration {
		root, err := url.Parse(m.SourceRoot)
		if err != nil {
			return fmt.Errorf("%s sourceRoot: %w", from, err)
		}
		if !root.IsAbs() && root.Host == "" && !path.IsAbs(root.Path) {
			relative, err := filepath.Rel(filepath.Dir(to), filepath.Join(filepath.Dir(from), filepath.FromSlash(root.Path)))
			if err != nil {
				return err
			}
			root.Path = filepath.ToSlash(relative) + "/"
			root.RawPath = ""
			m.SourceRoot = root.String()
		}
	} else {
		for i, src := range m.Sources {
			m.Sources[i] = filepath.ToSlash(filepath.Join(filepath.Dir(from), src))
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, out, 0o644)
}

func restoreMapSource(m *sourceMap, from, original string) error {
	logical := compilerPath(original)
	if logical == original {
		return nil
	}
	absoluteMap, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	base, err := url.Parse(m.SourceRoot)
	if err != nil {
		return err
	}
	base = (&url.URL{Scheme: "file", Path: filepath.ToSlash(absoluteMap)}).ResolveReference(base)
	absoluteLogical, err := filepath.Abs(logical)
	if err != nil {
		return err
	}
	absoluteOriginal, err := filepath.Abs(original)
	if err != nil {
		return err
	}
	for i, source := range m.Sources {
		entry, err := url.Parse(source)
		if err != nil {
			return err
		}
		resolved := base.ResolveReference(entry)
		if resolved.Scheme != "file" || resolved.Host != "" || filepath.Clean(filepath.FromSlash(resolved.Path)) != absoluteLogical {
			continue
		}
		if entry.IsAbs() || path.IsAbs(entry.Path) {
			entry.Path = filepath.ToSlash(absoluteOriginal)
		} else {
			relative, err := filepath.Rel(filepath.Dir(filepath.FromSlash(base.Path)), absoluteOriginal)
			if err != nil {
				return err
			}
			entry.Path = filepath.ToSlash(relative)
		}
		entry.RawPath = ""
		m.Sources[i] = entry.String()
	}
	return nil
}
