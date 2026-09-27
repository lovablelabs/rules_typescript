// Package tsconfig reads tsconfig.json files -- one file, or the chain its
// extends forms -- for Gazelle and the build actions alike.
package tsconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/mikn/rules_typescript/ts/tools/jsonc"
)

// File is the part of a tsconfig.json this ruleset reads.
type File struct {
	Extends         Extends         `json:"extends"`
	Include         *[]string       `json:"include"`
	Exclude         *[]string       `json:"exclude"`
	Files           *[]string       `json:"files"`
	CompilerOptions CompilerOptions `json:"compilerOptions"`
}

// CompilerOptions is the part of compilerOptions this ruleset reads.
type CompilerOptions struct {
	BaseURL string `json:"baseUrl"`
	Paths   *Paths `json:"paths"`
	// A pointer because "types": [] and no "types" key at all mean opposite
	// things to tsc: none, versus every @types package in scope.
	Types           *[]string `json:"types"`
	Jsx             string    `json:"jsx"`
	JsxImportSource string    `json:"jsxImportSource"`
	Module          string    `json:"module"`
}

// Paths keeps authored order because TypeScript chooses the first equal-prefix pattern.
type Paths []Path

type Path struct {
	Key    string
	Values []string
}

func (p *Paths) Entries() iter.Seq2[string, []string] {
	return func(yield func(string, []string) bool) {
		if p != nil {
			for _, entry := range *p {
				if !yield(entry.Key, entry.Values) {
					return
				}
			}
		}
	}
}

func (p *Paths) Index(key string) int {
	if p != nil {
		for i, entry := range *p {
			if entry.Key == key {
				return i
			}
		}
	}
	return -1
}

func (p *Paths) Get(key string) []string {
	if i := p.Index(key); i >= 0 {
		return (*p)[i].Values
	}
	return nil
}

func (p *Paths) Set(key string, values []string) {
	if i := p.Index(key); i >= 0 {
		(*p)[i].Values = values
	} else {
		*p = append(*p, Path{Key: key, Values: values})
	}
}

func (p *Paths) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("paths must be an object")
	}
	decoded := Paths{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		var values []string
		if err := decoder.Decode(&values); err != nil {
			return err
		}
		decoded.Set(key.(string), values)
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	*p = decoded
	return nil
}

func (p Paths) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, entry := range p {
		if i != 0 {
			out.WriteByte(',')
		}
		key, err := json.Marshal(entry.Key)
		if err != nil {
			return nil, err
		}
		values, err := json.Marshal(entry.Values)
		if err != nil {
			return nil, err
		}
		out.Write(key)
		out.WriteByte(':')
		out.Write(values)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// Extends is the list of configs a tsconfig inherits from, written as one
// specifier or, since TypeScript 5.0, an array of them.
type Extends []string

func (e *Extends) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*e = Extends{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*e = many
	return nil
}

// Read decodes one tsconfig.json without following its extends.
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := jsonc.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// Resolved is an extends chain flattened leaf-wins. A compilerOption keeps its
// writer's directory because a relative value resolves against that file, not the leaf.
type Resolved struct {
	// Extends belongs to the leaf and is not inherited.
	Extends Extends
	// Configs names the files read in extends order, including overridden bases and the leaf.
	Configs         []string
	BaseURL         string
	BaseURLDir      string
	Paths           *Paths
	PathsDir        string
	Types           *[]string
	Jsx             string
	JsxImportSource string
	Module          string
	// The root specs, each with its writer's directory: tsc rebases an
	// inherited files, include or exclude to the file that set it.
	Files, Include, Exclude          *[]string
	FilesDir, IncludeDir, ExcludeDir string
}

// Inputs reports whether a file in the chain sets include or files; without
// either, tsc enumerates the directory tree.
func (r *Resolved) Inputs() bool {
	return r.Include != nil || r.Files != nil
}

// Resolve reads path and, depth first, the configs it extends; the leaf wins.
// tsc replaces inherited compilerOptions keys whole: paths comes from one file.
func Resolve(path string) (*Resolved, error) {
	return ResolveWithAdmission(path, nil)
}

var ErrAdmission = errors.New("tsconfig admission rejected")

// ResolveWithAdmission checks every path before reading; admission errors abort the whole chain.
func ResolveWithAdmission(path string, admit func(string) error) (*Resolved, error) {
	return resolve(path, map[string]bool{}, admit)
}

func resolve(path string, ancestors map[string]bool, admit func(string) error) (*Resolved, error) {
	path = filepath.Clean(path)
	// Only an ancestor repeat is a cycle. A config reached twice down two
	// branches is read twice, because merge order decides which one wins.
	if ancestors[path] {
		return nil, errors.New("an extends cycle")
	}
	ancestors[path] = true
	defer delete(ancestors, path)

	if admit != nil {
		if err := admit(path); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAdmission, err)
		}
	}
	f, err := Read(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	resolved := &Resolved{Extends: f.Extends}
	for _, spec := range f.Extends {
		basePath, ok := ResolveExtends(dir, spec)
		if !ok {
			continue
		}
		base, err := resolve(basePath, ancestors, admit)
		if errors.Is(err, ErrAdmission) {
			return nil, err
		}
		if err != nil {
			log.Printf("tsconfig: %s extends %q, which is skipped: %v", path, spec, err)
			continue
		}
		resolved.override(base)
	}
	resolved.override(&Resolved{
		Configs:         []string{path},
		BaseURL:         f.CompilerOptions.BaseURL,
		BaseURLDir:      dir,
		Paths:           f.CompilerOptions.Paths,
		PathsDir:        dir,
		Types:           f.CompilerOptions.Types,
		Jsx:             f.CompilerOptions.Jsx,
		JsxImportSource: f.CompilerOptions.JsxImportSource,
		Module:          f.CompilerOptions.Module,
		Files:           f.Files,
		FilesDir:        dir,
		Include:         f.Include,
		IncludeDir:      dir,
		Exclude:         f.Exclude,
		ExcludeDir:      dir,
	})
	return resolved, nil
}

func (r *Resolved) override(other *Resolved) {
	r.Configs = append(r.Configs, other.Configs...)
	if other.BaseURL != "" {
		r.BaseURL, r.BaseURLDir = other.BaseURL, other.BaseURLDir
	}
	if other.Paths != nil {
		r.Paths, r.PathsDir = other.Paths, other.PathsDir
	}
	if other.Types != nil {
		r.Types = other.Types
	}
	if other.Jsx != "" {
		r.Jsx = other.Jsx
	}
	if other.JsxImportSource != "" {
		r.JsxImportSource = other.JsxImportSource
	}
	if other.Module != "" {
		r.Module = other.Module
	}
	if other.Files != nil {
		r.Files, r.FilesDir = other.Files, other.FilesDir
	}
	if other.Include != nil {
		r.Include, r.IncludeDir = other.Include, other.IncludeDir
	}
	if other.Exclude != nil {
		r.Exclude, r.ExcludeDir = other.Exclude, other.ExcludeDir
	}
}

// OxcEmits: oxc keeps the module syntax it reads, so it emits every ES kind
// and preserve; tsgo emits commonjs and the node kinds (package.json's format).
func OxcEmits(module string) bool {
	m := strings.ToLower(module)
	return m == "preserve" || strings.HasPrefix(m, "es")
}

// TopLevelAwait is TS1378's rule: tsc keeps a top-level await, never lowered,
// under these module kinds and a target of es2017 or later.
func TopLevelAwait(module, target string) bool {
	switch strings.ToLower(module) {
	case "es2022", "esnext", "system", "node16", "node18", "node20",
		"nodenext", "preserve":
	default:
		return false
	}
	t := strings.ToLower(target)
	if t == "esnext" {
		return true
	}
	year, err := strconv.Atoi(strings.TrimPrefix(t, "es"))
	return err == nil && year >= 2017
}

// ResolveExtends turns an extends value into a path on disk. A bare specifier
// resolves through node_modules, which this reader has no root for: skipped.
func ResolveExtends(dir, spec string) (string, bool) {
	if spec == "" {
		return "", false
	}
	relative := strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../")
	if !relative && !filepath.IsAbs(spec) {
		warnPackageFormExtends(dir, spec)
		return "", false
	}
	if !strings.HasSuffix(spec, ".json") {
		spec += ".json"
	}
	if !relative {
		return spec, true
	}
	return filepath.Join(dir, filepath.FromSlash(spec)), true
}

var packageFormExtendsWarned sync.Map

func warnPackageFormExtends(dir, spec string) {
	if _, warned := packageFormExtendsWarned.LoadOrStore(spec, true); warned {
		return
	}
	log.Printf("tsconfig: the tsconfig in %s extends %q, which resolves through node_modules; "+
		"only configs on disk are read, so any paths or baseUrl it sets are not seen here. "+
		"Inline them, or extend a checked-in config instead.", dir, spec)
}
