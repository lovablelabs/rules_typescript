// Package explainfiles reads tsgo's --explainFiles listing -- the program's
// files and why each is in it -- for Gazelle and the build actions alike.
package explainfiles

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Paths are as tsgo printed them. File listings are relative to its working
// directory; resolution traces can name absolute paths.
type Listing struct {
	Files       []string
	Roots       []string
	Edges       []Edge
	Types       []TypeEntry
	Implicit    []TypeEntry
	Diagnostics []string
	Unresolved  []Unresolved
	Resolutions []Resolution
	Candidates  []ResolutionCandidate
	// PackageTargets retains the selected file and failed probes after the
	// compiler enters package imports/exports resolution (past paths fallback).
	PackageTargets []Edge
}

// Traces retain module/type misses suppressed by @ts-ignore or noCheck;
// missing reference paths instead come from TS6053/TS6231 diagnostics.
type Unresolved struct {
	Kind            EdgeKind
	From, Specifier string
	Candidates      []FailedLookup
}

type FailedLookup struct {
	File, Candidate string
}

type Resolution struct {
	Edge
	Candidates    []FailedLookup
	Substitutions []string
	FromPackage   bool
}

type ResolutionCandidate struct {
	Kind                  EdgeKind
	From, Specifier, Path string
}

type resolutionAttempt struct {
	Unresolved
	substitutions []string
	packageStart  int
	candidate     string
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

var resolutionStart = regexp.MustCompile(`^======== Resolving (?:module '(.*?)' from '(.*?)'|type reference directive '(.*?)', containing file '(.*?)', root directory (?:'.*?'|not set))\. ========$`)
var resolutionEnd = regexp.MustCompile(`^======== (Module name|Type reference directive) '(.*?)' was (not resolved|successfully resolved to '(.*?)'(?: with Package ID '.*?')?(?:, primary: (?:true|false))?)\. ========$`)
var missingFileProbe = regexp.MustCompile(`^File '(.*?)' does not exist\.$`)
var packageTarget = regexp.MustCompile(`^Using '(imports|exports)' subpath '.*?' with target '(.*?)'\.$`)
var candidateLocation = regexp.MustCompile(`^Loading module as file / folder, candidate module location '(.*)', target file types: .+\.$`)
var pathSubstitution = regexp.MustCompile(`^Trying substitution '(.*?)', candidate module location: '(.*)'\.$`)
var missingReference = regexp.MustCompile(`^(.*?)\(\d+,\d+\): error TS(?:6053: File|6231: Could not resolve the path) '(.*?)' (?:not found|with the extensions: (.*?))\.$`)

type resolutionFrame struct {
	Kind                EdgeKind
	Specifier, From, To string
	Start, Resolved     bool
}

func parseResolutionFrame(line string) (resolutionFrame, bool) {
	if m := resolutionStart.FindStringSubmatch(line); m != nil {
		if strings.HasPrefix(line, "======== Resolving type reference directive '") {
			return resolutionFrame{Kind: TypeReference, Specifier: m[3], From: m[4], Start: true}, true
		}
		return resolutionFrame{Kind: Import, Specifier: m[1], From: m[2], Start: true}, true
	}
	if m := resolutionEnd.FindStringSubmatch(line); m != nil {
		kind := Import
		if m[1] == "Type reference directive" {
			kind = TypeReference
		}
		return resolutionFrame{Kind: kind, Specifier: m[2], To: m[4], Resolved: m[3] != "not resolved"}, true
	}
	return resolutionFrame{}, false
}

func Parse(text string) (*Listing, error) {
	l := &Listing{}
	var file, previous string
	var resolution *resolutionAttempt
	inDiagnostic := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "========") {
			frame, framed := parseResolutionFrame(line)
			switch {
			case framed && frame.Start:
				if resolution != nil {
					// Imports redirects print a nested start without their own end.
					redirect := packageTarget.FindStringSubmatch(previous)
					if resolution.Kind != Import || frame.Kind != Import || redirect == nil || redirect[1] != "imports" || frame.Specifier != redirect[2] || !filepath.IsAbs(frame.From) {
						return nil, fmt.Errorf("unrecognised nested compiler resolution trace: %q", line)
					}
				} else {
					if frame.Kind == Import && !filepath.IsAbs(frame.From) {
						return nil, fmt.Errorf("compiler resolution path is not absolute: %q", frame.From)
					}
					resolution = &resolutionAttempt{
						Unresolved:   Unresolved{Kind: frame.Kind, From: frame.From, Specifier: frame.Specifier},
						packageStart: -1,
					}
				}
			default:
				if resolution == nil || !framed || frame.Kind != resolution.Kind || frame.Specifier != resolution.Specifier {
					return nil, fmt.Errorf("unrecognised compiler resolution boundary: %q", line)
				}
				if frame.Resolved {
					l.Resolutions = append(l.Resolutions, Resolution{
						Edge:          Edge{Kind: resolution.Kind, From: resolution.From, Specifier: resolution.Specifier, To: frame.To},
						Candidates:    resolution.Candidates,
						Substitutions: resolution.substitutions,
						FromPackage:   resolution.packageStart >= 0,
					})
				} else {
					l.Unresolved = append(l.Unresolved, resolution.Unresolved)
				}
				if resolution.packageStart >= 0 {
					targets := resolution.Candidates[resolution.packageStart:]
					if frame.Resolved {
						targets = append(targets, FailedLookup{File: frame.To})
					}
					for _, target := range targets {
						l.PackageTargets = append(l.PackageTargets, Edge{Kind: resolution.Kind, From: resolution.From, Specifier: resolution.Specifier, To: target.File})
					}
				}
				resolution = nil
			}
			previous = ""
			continue
		}
		previous = line
		if strings.HasPrefix(line, "Loading module as file / folder") {
			candidate := candidateLocation.FindStringSubmatch(line)
			if resolution == nil || candidate == nil {
				return nil, fmt.Errorf("unframed or malformed compiler candidate: %q", line)
			}
			if resolution.Kind == Import && !filepath.IsAbs(candidate[1]) {
				return nil, fmt.Errorf("compiler resolution path is not absolute: %q", candidate[1])
			}
			resolution.candidate = candidate[1]
			l.Candidates = append(l.Candidates, ResolutionCandidate{Kind: resolution.Kind, From: resolution.From, Specifier: resolution.Specifier, Path: candidate[1]})
		}
		if resolution != nil {
			if strings.HasPrefix(line, "Loading module '") {
				resolution.candidate = ""
			}
			if resolution.Kind.ModuleSpecifier() && packageTarget.MatchString(line) {
				resolution.candidate = ""
				if resolution.packageStart < 0 {
					resolution.packageStart = len(resolution.Candidates)
				}
			} else if m := pathSubstitution.FindStringSubmatch(line); m != nil {
				resolution.substitutions = append(resolution.substitutions, m[1])
				resolution.candidate = m[2]
			} else if m := missingFileProbe.FindStringSubmatch(line); m != nil {
				resolution.Candidates = append(resolution.Candidates, FailedLookup{File: m[1], Candidate: resolution.candidate})
			}
			continue
		}
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
			if m := missingReference.FindStringSubmatch(line); m != nil {
				missing := Unresolved{Kind: Reference, From: m[1], Specifier: m[2]}
				if m[3] == "" {
					missing.Candidates = []FailedLookup{{File: m[2]}}
				} else {
					// TS6231 prints all extension groups; fileLoader.getSourceFileFromReference
					// probes only the first TS/JS group.
					for _, quoted := range strings.Split(m[3], ", ") {
						extension := strings.Trim(quoted, "'")
						switch extension {
						case ".ts", ".tsx", ".d.ts", ".js", ".jsx":
							missing.Candidates = append(missing.Candidates, FailedLookup{File: m[2] + extension})
						}
					}
				}
				l.Unresolved = append(l.Unresolved, missing)
			}
		default:
			file = line
			l.Files = append(l.Files, line)
			inDiagnostic = false
		}
	}
	if resolution != nil {
		return nil, fmt.Errorf("incomplete compiler resolution of %q from %s", resolution.Specifier, resolution.From)
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
