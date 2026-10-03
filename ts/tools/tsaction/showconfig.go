// The tsconfig step writes the program's tsconfig from the baseline, the user's
// file and `tsgo --showConfig`; each tsgo run's command line names its emit.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

type effectiveOptions struct {
	compilerOptions      map[string]json.RawMessage
	Target               string    `json:"target"`
	Jsx                  string    `json:"jsx"`
	JsxImportSource      string    `json:"jsxImportSource"`
	Module               string    `json:"module"`
	Types                *[]string `json:"types"`
	TypeRoots            []string  `json:"typeRoots"`
	IsolatedDeclarations bool      `json:"isolatedDeclarations"`
}

// oxcOptions is the options file: what oxc transforms with, and the module
// kind, which names the emitter and whether a top-level await stays.
type oxcOptions struct {
	Target          string `json:"target,omitempty"`
	Jsx             string `json:"jsx,omitempty"`
	JsxImportSource string `json:"jsxImportSource,omitempty"`
	Module          string `json:"module,omitempty"`
}

func (o oxcOptions) flags() []string {
	var out []string
	if o.Target != "" {
		out = append(out, "--target", o.Target)
	}
	if o.Jsx != "" {
		out = append(out, "--jsx", o.Jsx)
	}
	if o.JsxImportSource != "" {
		out = append(out, "--jsx-import-source", o.JsxImportSource)
	}
	if tsconfig.TopLevelAwait(o.Module, o.Target) {
		out = append(out, "--top-level-await")
	}
	return out
}

// showConfig is the chain's merged options and its root files in tsc's order,
// relative to project's directory.
func showConfig(tsgo, project string) (*effectiveOptions, []string, error) {
	cmd := exec.Command(tsgo, "--showConfig", "-p", project)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("%s --showConfig -p %s: %w\n%s%s",
			tsgo, project, err, out, stderr.Bytes())
	}
	options, roots, err := decodeShowConfig(out)
	if err != nil {
		return nil, nil, fmt.Errorf("%s --showConfig -p %s %w", tsgo, project, err)
	}
	return options, roots, nil
}

func decodeShowConfig(out []byte) (*effectiveOptions, []string, error) {
	var config struct {
		CompilerOptions json.RawMessage `json:"compilerOptions"`
		Files           []string        `json:"files"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return nil, nil, fmt.Errorf("printed no config (%v):\n%s", err, out)
	}
	if len(config.CompilerOptions) == 0 || string(config.CompilerOptions) == "null" || config.CompilerOptions[0] != '{' {
		return nil, nil, fmt.Errorf("unsupported --showConfig schema: expected a compilerOptions object; select a compiler that emits nested compilerOptions with string enum values")
	}
	var options effectiveOptions
	if err := json.Unmarshal(config.CompilerOptions, &options); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(config.CompilerOptions, &options.compilerOptions); err != nil {
		return nil, nil, err
	}
	return &options, config.Files, nil
}

// actionConfig is what the rule knows about one program and the user's
// tsconfig cannot: where the sandbox puts things and which tool declares it.
type actionConfig struct {
	tsgo, project, baseline, out, options string
	binDir                                string
	jsx, module                           string
	typesDeps                             stringList
	typeInputs                            stringList
	srcs                                  []string
	isolatedDeclarations                  bool
	libCheck                              bool
	sourceOnly                            bool
	javaScriptInputs                      bool
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

type tsconfigFile struct {
	CompilerOptions map[string]any `json:"compilerOptions"`
	Include         []string       `json:"include"`
	Files           []string       `json:"files,omitempty"`
	Exclude         []string       `json:"exclude"`
	References      []string       `json:"references"`
}

func writeTsconfig(args []string) error {
	var a actionConfig
	flags := flag.NewFlagSet("tsconfig", flag.ExitOnError)
	flags.StringVar(&a.tsgo, "tsgo", "", "the tsgo binary")
	flags.StringVar(&a.project, "tsconfig", "", "the user's tsconfig.json, when the target names one")
	flags.StringVar(&a.baseline, "baseline", "", "the ruleset's baseline options, extended before the user's file")
	flags.StringVar(&a.out, "out", "", "the tsconfig to write")
	flags.StringVar(&a.options, "options", "", "the oxc options file to write")
	flags.StringVar(&a.binDir, "bin_dir", "", "the output tree's root")
	flags.StringVar(&a.jsx, "jsx", "",
		"the ts_config's jsx: \"preserve\" names a .tsx's emit .jsx")
	flags.StringVar(&a.module, "module", "",
		"the ts_config's module: the chain's, when tsgo emits it")
	flags.Var(&a.typesDeps, "types_dep", "a direct @types dep's name, written "+
		"to types when the user's chain sets neither types nor typeRoots "+
		"(repeatable)")
	flags.Var(&a.typeInputs, "type_input", "a declared generated dependency file (repeatable)")
	flags.BoolVar(&a.isolatedDeclarations, "isolated_declarations", false, "oxc emits the declarations, so every export must be annotated")
	flags.BoolVar(&a.sourceOnly, "source_only", false, "the program publishes sources without emitting JavaScript")
	flags.BoolVar(&a.libCheck, "lib_check", false, "check the program's .d.ts closure too")
	flags.BoolVar(&a.javaScriptInputs, "javascript_inputs", false, "a source-mode dependency publishes JavaScript the program reads")
	if err := flags.Parse(args); err != nil {
		return err
	}
	a.srcs = flags.Args()
	for _, required := range []struct{ name, value string }{
		{"-tsgo", a.tsgo}, {"-baseline", a.baseline}, {"-out", a.out}, {"-options", a.options}, {"-bin_dir", a.binDir},
	} {
		if required.value == "" {
			return fmt.Errorf("tsconfig needs %s", required.name)
		}
	}

	var chain *tsconfig.Resolved
	if a.project != "" {
		var err error
		if chain, err = tsconfig.Resolve(a.project); err != nil {
			return err
		}
	}

	// showConfig reads a file: the chain's options and roots, tsc's default
	// include over the project rather than over the bin dir the file sits in.
	dir := path.Dir(a.out)
	first := map[string]any{"extends": a.extends(dir)}
	switch {
	case chain == nil:
		first["files"] = []string{}
	case !chain.Inputs():
		first["include"] = rebased(dir, path.Dir(a.project), []string{"**/*"})
	}
	if a.hasJavaScriptSrc() {
		first["compilerOptions"] = map[string]any{"allowJs": true}
	}
	if err := writeJSON(a.out, first); err != nil {
		return err
	}
	config, options, err := a.resolve(dir, chain)
	if err != nil {
		return errors.Join(err, os.Remove(a.out))
	}
	if err := writeJSON(a.out, config); err != nil {
		return err
	}
	return writeJSON(a.options, options)
}

func (a *actionConfig) resolve(dir string, chain *tsconfig.Resolved,
) (*tsconfigFile, oxcOptions, error) {
	effective, roots, err := showConfig(a.tsgo, a.out)
	if err != nil {
		return nil, oxcOptions{}, err
	}
	if !a.sourceOnly {
		if err := a.checkJsx(effective.Jsx); err != nil {
			return nil, oxcOptions{}, err
		}
		if err := a.checkModule(effective.Module); err != nil {
			return nil, oxcOptions{}, err
		}
	}
	config, err := a.build(effective, roots, chain, dir)
	if err != nil {
		return nil, oxcOptions{}, err
	}
	return config, oxcOptions{
		Target:          effective.Target,
		Jsx:             effective.Jsx,
		JsxImportSource: effective.JsxImportSource,
		Module:          effective.Module,
	}, nil
}

// The rule named a .tsx's emit from the ts_config before any action read the
// file; the two answers agree, or the edit that makes them agree is named.
func (a *actionConfig) checkJsx(effective string) error {
	if !a.hasTsxSrc() {
		return nil
	}
	declared := a.jsx == "preserve"
	preserve := strings.EqualFold(effective, "preserve")
	switch {
	case preserve && !declared:
		return fmt.Errorf("%s: jsx is \"preserve\", so a .tsx emits .jsx, and the "+
			"rule declared .js: declare it on the tsconfig's ts_config, "+
			"jsx = \"preserve\"", a.project)
	case declared && !preserve:
		return fmt.Errorf("%s: jsx is %q, so a .tsx emits .js, and the ts_config "+
			"declares jsx = \"preserve\": drop the attribute", a.project, effective)
	}
	return nil
}

// The rule declared the ES twins of a program tsgo emits from the ts_config
// before any action read the file; the two answers agree, or the edit is named.
func (a *actionConfig) checkModule(effective string) error {
	if !a.hasTsSrc() {
		return nil
	}
	want := ""
	if !tsconfig.OxcEmits(effective) {
		want = strings.ToLower(effective)
	}
	switch {
	case a.module == want:
		return nil
	case want == "":
		return fmt.Errorf("%s: module is %q, which oxc emits, and the ts_config "+
			"declares module = %q: drop the attribute", a.project, effective, a.module)
	case a.module == "":
		return fmt.Errorf("%s: module is %q, so tsgo emits the JavaScript and a "+
			"vitest test runs its ES twins, and the rule declared none: declare "+
			"it on the tsconfig's ts_config, module = %q", a.project, effective, want)
	default:
		return fmt.Errorf("%s: module is %q and the ts_config declares "+
			"module = %q: declare module = %q", a.project, effective, a.module, want)
	}
}

func (a *actionConfig) extends(dir string) []string {
	out := []string{fileRelative(dir, a.baseline)}
	if a.project != "" {
		out = append(out, fileRelative(dir, a.project))
	}
	return out
}

func (a *actionConfig) build(effective *effectiveOptions, roots []string,
	chain *tsconfig.Resolved, dir string) (*tsconfigFile, error) {
	types, typesRoots, err := a.types(effective, dir)
	if err != nil {
		return nil, err
	}
	declaration := a.isolatedDeclarations || effective.IsolatedDeclarations
	opts := make(map[string]any, len(effective.compilerOptions))
	for name, value := range effective.compilerOptions {
		opts[name] = value
	}
	for name, value := range map[string]any{
		"rootDirs":    []string{relativePath(dir, ""), relativePath(dir, a.binDir)},
		"rootDir":     relativePath(dir, ""),
		"composite":   false,
		"incremental": false,
		"declaration": declaration,
		// Off under the composite turned off; null unsets a path.
		"declarationMap":      false,
		"emitDeclarationOnly": false,
		"declarationDir":      nil,
	} {
		opts[name] = value
	}
	if types != nil {
		opts["types"] = types
	}
	if a.hasJavaScriptSrc() {
		opts["allowJs"] = true
	}
	if chain != nil {
		if paths := a.paths(chain, dir); paths != nil {
			opts["paths"] = paths
		}
	}
	if a.isolatedDeclarations {
		opts["isolatedDeclarations"] = true
	}
	if a.libCheck {
		opts["skipLibCheck"] = false
	}

	files, include, exclude := a.chainRoots(chain, dir)
	projectSpec := func(spec string) string {
		if path.IsAbs(spec) {
			return spec
		}
		return fileRelative(dir, compilerPath(path.Join(dir, spec)))
	}
	project := func(specs []string) {
		for i, spec := range specs {
			specs[i] = projectSpec(spec)
		}
	}
	project(files)
	project(include)
	project(exclude)
	project(typesRoots)
	named := make(map[string]bool, len(roots))
	for _, p := range roots {
		named[path.Clean(projectSpec(p))] = true
	}
	// tsc drops the lower-priority extension of a pair -- an .mjs beside its
	// .d.mts -- from what include names, never from files.
	src := make(map[string]bool, len(a.srcs))
	for _, s := range a.srcs {
		rel := fileRelative(dir, compilerPath(s))
		src[path.Clean(rel)] = true
		if !named[path.Clean(rel)] {
			include = append(include, rel)
		}
	}
	for _, p := range typesRoots {
		if !src[path.Clean(p)] {
			include = append(include, p)
		}
	}
	return &tsconfigFile{
		CompilerOptions: opts,
		Include:         include,
		Files:           files,
		Exclude:         exclude,
		References:      []string{},
	}, nil
}

// chainRoots is the chain's files, include and exclude, each from its writer's
// directory as tsc reads it; no exclude is `[]`: tsc's default names outDir.
func (a *actionConfig) chainRoots(chain *tsconfig.Resolved, dir string,
) (files, include, exclude []string) {
	files, include, exclude = []string{}, []string{}, []string{}
	if chain == nil {
		return files, include, exclude
	}
	if chain.Files != nil {
		files = rebased(dir, chain.FilesDir, *chain.Files)
	}
	switch {
	case chain.Include != nil:
		include = rebased(dir, chain.IncludeDir, *chain.Include)
	case chain.Files == nil:
		include = rebased(dir, path.Dir(a.project), []string{"**/*"})
	}
	if chain.Exclude != nil {
		exclude = rebased(dir, chain.ExcludeDir, *chain.Exclude)
	}
	return files, include, exclude
}

// rebased spells each spec written in from relative to dir.
func rebased(dir, from string, specs []string) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		if path.IsAbs(spec) {
			out = append(out, spec)
			continue
		}
		spec = relativePath(dir, path.Join(from, spec))
		out = append(out, explicitlyRelative(spec))
	}
	return out
}

// A .ts or .tsx src has an emit; a declaration has none to name.
func (a *actionConfig) hasTsSrc() bool {
	for _, src := range a.srcs {
		switch path.Ext(src) {
		case ".ts", ".tsx":
			if !strings.HasSuffix(src, ".d.ts") {
				return true
			}
		}
	}
	return false
}

func (a *actionConfig) hasTsxSrc() bool {
	for _, src := range a.srcs {
		if path.Ext(src) == ".tsx" {
			return true
		}
	}
	return false
}

// A JavaScript src sets allowJs; without it a pattern skips the file and a
// root entry for it is TS6504.
func (a *actionConfig) hasJavaScriptSrc() bool {
	if a.javaScriptInputs {
		return true
	}
	for _, src := range a.srcs {
		if isJavaScript(src) {
			return true
		}
	}
	return false
}

func isJavaScript(file string) bool {
	switch path.Ext(file) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// paths is the user's map read from the directory of the chain file that set
// it -- which showConfig does not print -- with a bin-dir twin per value.
func (a *actionConfig) paths(chain *tsconfig.Resolved, dir string) map[string][]string {
	if chain.Paths == nil {
		return nil
	}
	out := make(map[string][]string, len(chain.Paths))
	for key, values := range chain.Paths {
		rewritten := make([]string, 0, 2*len(values))
		for _, value := range values {
			if path.IsAbs(value) {
				rewritten = append(rewritten, value)
				continue
			}
			target := compilerPath(path.Join(chain.PathsDir, value))
			rewritten = append(rewritten,
				explicitlyRelative(relativePath(dir, target)),
				explicitlyRelative(relativePath(dir, path.Join(a.binDir, target))))
		}
		out[key] = rewritten
	}
	return out
}

// types keeps the user's names, or the direct @types deps' when the chain sets
// no types and no typeRoots; each path-shaped entry is returned as a root file.
func (a *actionConfig) types(effective *effectiveOptions, dir string,
) (names, roots []string, err error) {
	if effective.Types == nil {
		if effective.TypeRoots != nil {
			return nil, nil, nil
		}
		return append([]string{}, a.typesDeps...), nil, nil
	}
	projectDir := "."
	if a.project != "" {
		projectDir = path.Dir(a.project)
	}
	names = make([]string, 0, len(*effective.Types))
	declared := make(map[string]bool, len(a.typeInputs))
	for _, file := range a.typeInputs {
		declared[path.Clean(file)] = true
	}
	exists := func(file string) bool { return declared[path.Clean(file)] || isFile(file) }
	for _, entry := range *effective.Types {
		if !isRelative(entry) {
			names = append(names, entry)
			continue
		}
		target := path.Join(projectDir, entry)
		file := typesEntryFile(target, exists)
		if file == "" {
			file = typesEntryFile(path.Join(a.binDir, target), exists)
		}
		if file == "" {
			return nil, nil, fmt.Errorf("compilerOptions.types entry %q in %s "+
				"names %s, which no input of this action sits at: not in the "+
				"source tree, not under %s.\nA declaration this program names "+
				"is a src of the target or an output of one of its deps.",
				entry, a.project, target, a.binDir)
		}
		roots = append(roots, fileRelative(dir, file))
	}
	return names, roots, nil
}

var typesEntryExtensions = []string{".ts", ".tsx", ".d.ts", ".mts", ".d.mts", ".cts", ".d.cts"}

// typesEntryFile is tsc's lookup for a path-shaped entry: the path as a file,
// a TypeScript or declaration extension added, or a directory's index.d.ts.
func typesEntryFile(p string, exists func(string) bool) string {
	if exists(p) {
		return p
	}
	for _, ext := range typesEntryExtensions {
		if exists(p + ext) {
			return p + ext
		}
	}
	if index := path.Join(p, "index.d.ts"); exists(index) {
		return index
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isRelative(p string) bool {
	return p == "." || p == ".." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../")
}

func fileRelative(dir, file string) string {
	return explicitlyRelative(relativePath(dir, path.Dir(file)) + "/" + path.Base(file))
}

// relativePath is the ruleset's spelling for a /-separated path from one
// directory to another: "." for the same one, and "" or "." name the exec root.
func relativePath(from, to string) string {
	fromParts, toParts := segments(from), segments(to)
	common := 0
	for common < len(fromParts) && common < len(toParts) && fromParts[common] == toParts[common] {
		common++
	}
	parts := make([]string, 0, len(fromParts)-common+len(toParts)-common)
	for range fromParts[common:] {
		parts = append(parts, "..")
	}
	parts = append(parts, toParts[common:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

func segments(p string) []string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

// explicitlyRelative spells a path so TypeScript reads it as one: a bare
// segment is a package name to `paths` (TS5090), and `.bazel/x` is a directory.
func explicitlyRelative(p string) string {
	if p == "." || p == ".." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return p
	}
	return "./" + p
}

func writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(name, append(data, '\n'), 0o644)
}
