// Package explainfiles reads tsgo's --explainFiles listing -- the program's
// files and why each is in it -- for Gazelle and the build actions alike.
package explainfiles

import (
	"fmt"
	"regexp"
	"strings"
)

// Paths are as tsgo printed them: relative to its working directory.
type Listing struct {
	Files       []string
	Roots       []string
	Edges       []Edge
	Types       []TypeEntry
	Implicit    []TypeEntry
	Diagnostics []string
}

type EdgeKind int

const (
	Import        EdgeKind = iota
	Reference              // /// <reference path>
	TypeReference          // /// <reference types>
	Augmentation           // declare module "x"
)

// ModuleSpecifier is whether the edge's Specifier is a module specifier as
// the source wrote it -- an import's or an augmentation's.
func (k EdgeKind) ModuleSpecifier() bool {
	return k == Import || k == Augmentation
}

type Edge struct {
	Kind      EdgeKind
	From      string
	To        string
	Specifier string
}

// A compilerOptions.types entry as written, and the file it resolved to.
type TypeEntry struct {
	Entry string
	File  string
}

// A file prints on its own line, each reason for it indented three spaces; a
// diagnostic's continuation lines are indented two.
var diagnosticLine = regexp.MustCompile(`^(?:\S.*\(\d+,\d+\): )?error TS\d+: `)

func Parse(text string) (*Listing, error) {
	l := &Listing{}
	var file string
	inDiagnostic := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
		case inDiagnostic && strings.HasPrefix(line, "  "):
			l.Diagnostics[len(l.Diagnostics)-1] += "\n" + line
		case strings.HasPrefix(line, "   "):
			if file == "" {
				return nil, fmt.Errorf("a reason before any file line: %q", line)
			}
			if err := readReason(l, file, line[3:]); err != nil {
				return nil, err
			}
		case strings.Contains(line, "error TS") && diagnosticLine.MatchString(line):
			l.Diagnostics = append(l.Diagnostics, line)
			inDiagnostic = true
		default:
			file = line
			l.Files = append(l.Files, line)
			inDiagnostic = false
		}
	}
	return l, nil
}

func readReason(l *Listing, file, reason string) error {
	for _, f := range reasonForms {
		if !f.admits(reason) {
			continue
		}
		if m := f.re.FindStringSubmatch(reason); m != nil {
			f.read(l, file, m)
			return nil
		}
	}
	return fmt.Errorf("%s: unrecognised --explainFiles reason %q; the grammar "+
		"is tsgo's, pinned in ts/tools/explainfiles", file, reason)
}

// tsgo's --explainFiles templates for a program without project references,
// from its string table, each with what it says of its file.
type reasonForm struct {
	re   *regexp.Regexp
	read func(l *Listing, file string, m []string)
	// Literal text every match holds: its leading and trailing run and each run between groups.
	prefix, suffix string
	literals       []string
}

func (f reasonForm) admits(reason string) bool {
	if !strings.HasPrefix(reason, f.prefix) || !strings.HasSuffix(reason, f.suffix) {
		return false
	}
	for _, literal := range f.literals {
		if !strings.Contains(reason, literal) {
			return false
		}
	}
	return true
}

// requiredLiterals is the literal runs outside the groups of a pattern with no escape,
// class, repetition count or top-level alternation; any other pattern requires none.
func requiredLiterals(pattern string) (prefix, suffix string, literals []string) {
	if strings.ContainsAny(pattern, `\[]{}`) {
		return "", "", nil
	}
	var run []byte
	depth, start := 0, 0
	flush := func(end int) {
		if len(run) > 0 {
			if start == 0 {
				prefix = string(run)
			}
			if end == len(pattern) {
				suffix = string(run)
			}
			literals = append(literals, string(run))
		}
		run = run[:0]
	}
	for i := 0; i < len(pattern); i++ {
		switch ch := pattern[i]; {
		case ch == '(':
			flush(-1)
			depth++
		case ch == ')':
			depth--
			start = -1
		case depth > 0:
		case ch == '|':
			return "", "", nil
		case ch == '*' || ch == '?' || ch == '+':
			if len(run) > 0 {
				run = run[:len(run)-1]
			}
			flush(-1)
			start = -1
		case ch == '.' || ch == '^' || ch == '$':
			flush(-1)
			start = -1
		default:
			if len(run) == 0 && start != 0 {
				start = i
			}
			run = append(run, ch)
		}
	}
	flush(len(pattern))
	return prefix, suffix, literals
}

// A quoted value runs to the quote before its form's next literal token, so a
// quote inside a path parses; a family's longer forms come first likewise.
const (
	quoted    = `'(.*?)'`
	specifier = `(".*?"|'.*?')`
	packageID = ` with packageId '.*?'`
)

func form(pattern string, read func(l *Listing, file string, m []string),
) reasonForm {
	prefix, suffix, literals := requiredLiterals(pattern)
	return reasonForm{re: regexp.MustCompile("^" + pattern + "$"), read: read, prefix: prefix, suffix: suffix, literals: literals}
}

func asRoot(l *Listing, file string, _ []string) {
	l.Roots = append(l.Roots, file)
}

func asNothing(*Listing, string, []string) {}

func asEdge(kind EdgeKind) func(l *Listing, file string, m []string) {
	return func(l *Listing, file string, m []string) {
		spec := m[1]
		if kind.ModuleSpecifier() {
			spec = spec[1 : len(spec)-1]
		}
		l.Edges = append(l.Edges, Edge{Kind: kind, From: m[2], To: file,
			Specifier: spec})
	}
}

func asTypeEntry(l *Listing, file string, m []string) {
	l.Types = append(l.Types, TypeEntry{Entry: m[1], File: file})
}

func asImplicit(l *Listing, file string, m []string) {
	l.Implicit = append(l.Implicit, TypeEntry{Entry: m[1], File: file})
}

var reasonForms = []reasonForm{
	form(`Imported via `+specifier+` from file `+quoted+packageID+
		` to import 'jsx' and 'jsxs' factory functions`, asEdge(Import)),
	form(`Imported via `+specifier+` from file `+quoted+packageID+
		` to import 'importHelpers' as specified in compilerOptions`,
		asEdge(Import)),
	form(`Imported via `+specifier+` from file `+quoted+packageID,
		asEdge(Import)),
	form(`Imported via `+specifier+` from file `+quoted+
		` to import 'jsx' and 'jsxs' factory functions`, asEdge(Import)),
	form(`Imported via `+specifier+` from file `+quoted+
		` to import 'importHelpers' as specified in compilerOptions`,
		asEdge(Import)),
	form(`Imported via `+specifier+` from file `+quoted, asEdge(Import)),
	form(`Augmented via `+specifier+` from file `+quoted+packageID,
		asEdge(Augmentation)),
	form(`Augmented via `+specifier+` from file `+quoted, asEdge(Augmentation)),
	form(`Referenced via `+quoted+` from file `+quoted, asEdge(Reference)),
	form(`Type library referenced via `+quoted+` from file `+quoted+packageID,
		asEdge(TypeReference)),
	form(`Type library referenced via `+quoted+` from file `+quoted,
		asEdge(TypeReference)),
	form(`Entry point of type library `+quoted+` specified in compilerOptions`+
		packageID, asTypeEntry),
	form(`Entry point of type library `+quoted+` specified in compilerOptions`,
		asTypeEntry),
	form(`Entry point for implicit type library `+quoted+packageID, asImplicit),
	form(`Entry point for implicit type library `+quoted, asImplicit),
	form(`Matched by include pattern `+quoted+` in `+quoted, asRoot),
	form(`Matched by default include pattern `+quoted, asRoot),
	form(`Part of 'files' list in tsconfig.json`, asRoot),
	form(`Root file specified for compilation`, asRoot),
	form(`Library referenced via `+quoted+` from file `+quoted, asNothing),
	form(`Library `+quoted+` specified in compilerOptions`, asNothing),
	form(`Default library for target `+quoted, asNothing),
	form(`Default library`, asNothing),
	form(`File is ECMAScript module because `+quoted+
		` has field "type" with value "module"`, asNothing),
	form(`File is CommonJS module because `+quoted+
		` has field "type" whose value is not "module"`, asNothing),
	form(`File is CommonJS module because `+quoted+
		` does not have field "type"`, asNothing),
	form(`File is CommonJS module because 'package.json' was not found`,
		asNothing),
	form(`File redirects to file `+quoted, asNothing),
}
