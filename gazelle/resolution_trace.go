package typescript

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

type resolutionCandidate struct {
	from, specifier, path string
	file                  bool
	metadata              bool
	kind                  explainfiles.EdgeKind
	block                 int
	resolved, skipped     string
}

var typeTraceStart = regexp.MustCompile(`^======== Resolving type reference directive '(.*?)', .* ========$`)
var typeTraceEnd = regexp.MustCompile(`^======== Type reference directive '(.*?)' (?:was not resolved\.|was successfully resolved to '(.*?)'(?: with Package ID '.*?')?, primary: .*?\.) ========$`)
var typeContainingFileTrace = regexp.MustCompile(`containing file '(.*?)'`)
var importsTrace = regexp.MustCompile(`^Using 'imports' subpath '.*' with target '(.*)'\.$`)

// moduleTraceStart matches `^======== Resolving module '(.*)' from '(.*)'\. ========$`.
func moduleTraceStart(line string) []string {
	value, ok := traceField(line, "======== Resolving module '", "'. ========")
	i := strings.LastIndex(value, "' from '")
	if !ok || i < 0 {
		return nil
	}
	return []string{line, value[:i], value[i+len("' from '"):]}
}

// moduleTraceEnd matches `^======== Module name '(.*)' (?:was not resolved\.|was successfully
// resolved to '(.*?)'(?: with Package ID '.*?')?\.) ========$`, its greedy and lazy groups included.
func moduleTraceEnd(line string) []string {
	value, ok := traceField(line, "======== Module name '", " ========")
	if !ok {
		return nil
	}
	const resolvedTo, packageID = "was successfully resolved to '", " with Package ID '"
	for end := len(value); ; {
		i := strings.LastIndex(value[:end], "' was ")
		if i < 0 {
			return nil
		}
		end = i
		rest := value[i+len("' "):]
		if rest == "was not resolved." {
			return []string{line, value[:i], ""}
		}
		body, ok := traceField(rest, resolvedTo, ".")
		if !ok {
			continue
		}
		for j := 0; j < len(body); j++ {
			if body[j] != '\'' {
				continue
			}
			after := body[j+1:]
			if after == "" || len(after) > len(packageID) && strings.HasPrefix(after, packageID) && strings.HasSuffix(after, "'") {
				return []string{line, value[:i], body[:j]}
			}
		}
	}
}

// candidateTrace matches `^Loading module as file / folder, candidate module location '(.*)', target file types: (.+)\.$`.
func candidateTrace(line string) []string {
	value, ok := traceField(line, "Loading module as file / folder, candidate module location '", ".")
	if !ok {
		return nil
	}
	const types = "', target file types: "
	for end := len(value); ; {
		i := strings.LastIndex(value[:end], types)
		if i < 0 {
			return nil
		}
		if len(value)-i-len(types) >= 1 {
			return []string{line, value[:i], value[i+len(types):]}
		}
		end = i
	}
}

// traceField is the `(.*)` of a `^prefix(.*)suffix$` trace line; a split line holds no newline for `.` to refuse.
func traceField(line, prefix, suffix string) (string, bool) {
	if len(line) < len(prefix)+len(suffix) || !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return "", false
	}
	return line[len(prefix) : len(line)-len(suffix)], true
}

func missingFileTrace(line string) (string, bool) {
	if value, ok := traceField(line, "File '", "' does not exist."); ok {
		return value, true
	}
	return traceField(line, "File '", "' does not exist according to earlier cached lookups.")
}

// The compiler's own spelling, `Found 'package.json' at '...'.`, whose `.` matches any one character.
func foundPackageTrace(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "Found 'package")
	if !ok || rest == "" {
		return "", false
	}
	_, width := utf8.DecodeRuneInString(rest)
	return traceField(rest[width:], "json' at '", "'.")
}

// resolutionFacts is what every module resolution in a listing said about how
// relative and paths-mapped specifiers resolve, so patchListing can resolve a
// new one the same way.
type resolutionFacts struct {
	Kinds, RelativeTargets []string
	RootDirs               bool
	// pattern\x00substitution\x00base for each traced paths substitution, and its candidates' target file types.
	Substitutions, SubstitutionTargets []string
}

func (f *resolutionFacts) add(list *[]string, value string) {
	if !slices.Contains(*list, value) {
		*list = append(*list, strings.Clone(value))
	}
}

// Trace arrays are emitted serially by the compiler's filesparser, before its listing.
func splitResolutionTrace(root, output string) (string, []resolutionCandidate, error) {
	return splitTrace(root, output, &resolutionFacts{})
}

func splitTrace(root, output string, facts *resolutionFacts) (string, []resolutionCandidate, error) {
	t := newTraceSplitter(root, facts)
	for rest := output; rest != ""; {
		raw := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			raw = rest[:i+1]
		}
		rest = rest[len(raw):]
		if err := t.line(raw); err != nil {
			return "", nil, err
		}
	}
	return t.finish()
}

// traceSplitter takes tsgo's output a line at a time, keeping the listing's
// lines and the trace's candidates. What it keeps past a line is copied or
// interned: the buffer a line is in is reused once it is handled.
type traceSplitter struct {
	root, under                                                  string
	facts                                                        *resolutionFacts
	listing                                                      strings.Builder
	candidates                                                   []resolutionCandidate
	sawCandidate                                                 bool
	kind, specifier, from, previous, jsonModule, moduleCandidate string
	block, blockStart                                            int
	// The paths pattern and substitution whose candidate the next candidate line announces.
	pattern, substitution, substituted string
	rels                               *sync.Map
}

func newTraceSplitter(root string, facts *resolutionFacts) *traceSplitter {
	rels, _ := outsideRels.LoadOrStore(root, &sync.Map{})
	return &traceSplitter{root: root, under: filepath.Clean(root) + string(filepath.Separator), facts: facts, rels: rels.(*sync.Map)}
}

// outsideRels holds, per root, the relative path of each path the fast path of
// relative cannot cut, such as the lib files every listing resolves.
var outsideRels sync.Map

func (t *traceSplitter) relative(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("compiler resolution path is not absolute: %q", value)
	}
	if len(value) > len(t.under) && strings.HasPrefix(value, t.under) && filepath.Clean(value) == value {
		return internString(filepath.ToSlash(value[len(t.under):])), nil
	}
	if rel, ok := t.rels.Load(value); ok {
		return rel.(string), nil
	}
	rel, err := filepath.Rel(t.root, value)
	if err != nil {
		return internString(filepath.ToSlash(rel)), err
	}
	kept := internString(filepath.ToSlash(rel))
	t.rels.Store(strings.Clone(value), kept)
	return kept, nil
}

func (t *traceSplitter) line(raw string) error {
	facts := t.facts
	line := strings.TrimRight(raw, "\r\n")
	if strings.HasPrefix(line, "========") {
		var start, typeStart []string
		if strings.HasPrefix(line, "======== Resolving module '") {
			start = moduleTraceStart(line)
		}
		if start == nil && strings.HasPrefix(line, "======== Resolving type reference directive '") {
			typeStart = typeTraceStart.FindStringSubmatch(line)
		}
		switch {
		case start != nil || typeStart != nil:
			if t.kind != "" {
				redirect := importsTrace.FindStringSubmatch(t.previous)
				if t.kind != "module" || start == nil || redirect == nil || start[1] != redirect[1] || !filepath.IsAbs(start[2]) {
					return fmt.Errorf("unrecognised nested compiler resolution trace: %q", line)
				}
				t.previous = ""
				return nil
			}
			t.moduleCandidate, t.sawCandidate = "", false
			t.pattern, t.substitution, t.substituted = "", "", ""
			t.blockStart = len(t.candidates)
			if m := start; m != nil {
				t.kind, t.specifier = "module", internString(m[1])
				var err error
				t.from, err = t.relative(m[2])
				if err != nil {
					return err
				}
			} else {
				t.kind, t.specifier = "type", internString(typeStart[1])
				if m := typeContainingFileTrace.FindStringSubmatch(line); m != nil {
					var err error
					t.from, err = t.relative(m[1])
					if err != nil {
						return err
					}
				}
			}
		default:
			var m []string
			if t.kind == "type" {
				m = typeTraceEnd.FindStringSubmatch(line)
			} else {
				m = moduleTraceEnd(line)
			}
			if t.kind == "" || m == nil || m[1] != t.specifier {
				return fmt.Errorf("unrecognised compiler resolution boundary: %q", line)
			}
			resolved := ""
			if m[2] != "" {
				var err error
				resolved, err = t.relative(m[2])
				if err != nil {
					return err
				}
				t.candidates = append(t.candidates, resolutionCandidate{from: t.from, specifier: t.specifier, path: resolved, file: true})
			}
			for i := t.blockStart; i < len(t.candidates); i++ {
				t.candidates[i].block, t.candidates[i].resolved = t.block, resolved
				if t.kind == "type" {
					t.candidates[i].kind = explainfiles.TypeReference
				}
			}
			t.block++
			t.kind, t.specifier, t.from = "", "", ""
		}
		t.previous = ""
		return nil
	}
	t.previous = ""
	if strings.HasPrefix(line, "Using 'imports' subpath '") {
		t.previous = strings.Clone(line)
	}
	missing, isMissing := missingFileTrace(line)
	if value, ok := traceField(line, "File name '", "' has a '.json' extension - stripping it."); t.kind == "module" && ok {
		t.jsonModule = strings.Clone(value)
	} else if !isMissing {
		t.jsonModule = ""
	}
	if strings.HasPrefix(line, "Loading module as file / folder") {
		m := candidateTrace(line)
		if t.kind == "" || m == nil {
			return fmt.Errorf("unframed or malformed compiler candidate: %q", line)
		}
		if t.kind == "module" {
			candidate, err := t.relative(m[1])
			if err != nil {
				return err
			}
			if !t.sawCandidate && relativeSpecifier(t.specifier) {
				facts.add(&facts.RelativeTargets, m[2])
			}
			if t.substitution != "" {
				if base, ok := substitutionBase(candidate, t.substituted); ok {
					facts.add(&facts.Substitutions, t.pattern+"\x00"+t.substitution+"\x00"+base)
					facts.add(&facts.SubstitutionTargets, m[2])
				}
				t.substitution = ""
			}
			t.sawCandidate = true
			// This announcement precedes file probes; only a later skipped
			// directory establishes where a cold tree fits in their order.
			t.moduleCandidate = candidate
		}
	}
	if t.kind == "module" {
		if value, ok := traceField(line, "Module name '", "'."); ok {
			if i := strings.LastIndex(value, "', matched pattern '"); i >= 0 && value[:i] == t.specifier {
				t.pattern = internString(value[i+len("', matched pattern '"):])
			}
		}
		if value, ok := traceField(line, "Trying substitution '", "'."); ok && t.pattern != "" {
			if i := strings.LastIndex(value, "', candidate module location: '"); i >= 0 {
				t.substitution, t.substituted = internString(value[:i]), strings.Clone(value[i+len("', candidate module location: '"):])
			}
		}
		if strings.Contains(line, "odule resolution kind") {
			facts.add(&facts.Kinds, line)
		}
		if strings.HasPrefix(line, "'rootDirs' option is set") {
			facts.RootDirs = true
		}
	}
	if t.kind != "" {
		if isMissing {
			candidate, err := t.relative(missing)
			if err != nil {
				return err
			}
			metadata := path.Base(candidate) == "package.json" && missing != t.jsonModule
			if firstParty(candidate) && (metadata || programCandidate(candidate) || t.kind == "module" && path.Ext(candidate) == ".json") {
				t.candidates = append(t.candidates, resolutionCandidate{from: t.from, specifier: t.specifier, path: candidate, file: !metadata, metadata: metadata})
			}
		}
		found, ok := foundPackageTrace(line)
		if !ok {
			found, ok = traceField(line, "File '", "' exists according to earlier cached lookups.")
		}
		if ok {
			candidate, err := t.relative(found)
			if err != nil {
				return err
			}
			if firstParty(candidate) && path.Base(candidate) == "package.json" {
				t.candidates = append(t.candidates, resolutionCandidate{from: t.from, specifier: t.specifier, path: candidate, metadata: true})
			}
		}
		if missingDirectory, ok := traceField(line, "Directory '", "' does not exist, skipping all lookups in it."); t.kind == "module" && ok && t.moduleCandidate != "" {
			directory, err := t.relative(missingDirectory)
			if err != nil {
				return err
			}
			if parentDir(t.moduleCandidate) == directory || t.moduleCandidate == directory {
				t.candidates = append(t.candidates, resolutionCandidate{from: t.from, specifier: t.specifier, path: t.moduleCandidate, skipped: directory})
			}
		}
	}
	if t.kind == "" {
		t.listing.WriteString(raw)
	}
	return nil
}

func (t *traceSplitter) finish() (string, []resolutionCandidate, error) {
	if t.kind != "" {
		return "", nil, fmt.Errorf("unfinished compiler resolution trace for %q", t.specifier)
	}
	return t.listing.String(), t.candidates, nil
}

// substitutionBase is the directory a substituted path resolved against: the
// candidate without the path's normalised tail, "" for the repository root.
func substitutionBase(candidate, substituted string) (string, bool) {
	tail := path.Clean(substituted)
	if tail == "." || path.IsAbs(tail) || tail == ".." || strings.HasPrefix(tail, "../") {
		return "", false
	}
	if candidate == tail {
		return "", true
	}
	base, ok := strings.CutSuffix(candidate, "/"+tail)
	return base, ok && base != ""
}

// relativeSpecifier is tsgo's IsExternalModuleNameRelative: ".", "..", or a path
// starting with either, or a rooted one.
func relativeSpecifier(spec string) bool {
	return spec == "." || spec == ".." || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "/")
}
