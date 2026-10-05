package typescript

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
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

// Trace arrays are emitted serially by the compiler's filesparser, before its listing.
func splitResolutionTrace(root, output string) (string, []resolutionCandidate, error) {
	var listing strings.Builder
	var candidates []resolutionCandidate
	var kind, specifier, from, previous, jsonModule, moduleCandidate string
	block, blockStart := 0, 0
	under := filepath.Clean(root) + string(filepath.Separator)
	relative := func(value string) (string, error) {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("compiler resolution path is not absolute: %q", value)
		}
		if len(value) > len(under) && strings.HasPrefix(value, under) && filepath.Clean(value) == value {
			return filepath.ToSlash(value[len(under):]), nil
		}
		rel, err := filepath.Rel(root, value)
		return filepath.ToSlash(rel), err
	}
	for rest := output; rest != ""; {
		raw := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			raw = rest[:i+1]
		}
		rest = rest[len(raw):]
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
				if kind != "" {
					redirect := importsTrace.FindStringSubmatch(previous)
					if kind != "module" || start == nil || redirect == nil || start[1] != redirect[1] || !filepath.IsAbs(start[2]) {
						return "", nil, fmt.Errorf("unrecognised nested compiler resolution trace: %q", line)
					}
					previous = ""
					continue
				}
				moduleCandidate = ""
				blockStart = len(candidates)
				if m := start; m != nil {
					kind, specifier = "module", m[1]
					var err error
					from, err = relative(m[2])
					if err != nil {
						return "", nil, err
					}
				} else {
					kind, specifier = "type", typeStart[1]
					if m := typeContainingFileTrace.FindStringSubmatch(line); m != nil {
						var err error
						from, err = relative(m[1])
						if err != nil {
							return "", nil, err
						}
					}
				}
			default:
				var m []string
				if kind == "type" {
					m = typeTraceEnd.FindStringSubmatch(line)
				} else {
					m = moduleTraceEnd(line)
				}
				if kind == "" || m == nil || m[1] != specifier {
					return "", nil, fmt.Errorf("unrecognised compiler resolution boundary: %q", line)
				}
				resolved := ""
				if m[2] != "" {
					var err error
					resolved, err = relative(m[2])
					if err != nil {
						return "", nil, err
					}
					candidates = append(candidates, resolutionCandidate{from: from, specifier: specifier, path: resolved, file: true})
				}
				for i := blockStart; i < len(candidates); i++ {
					candidates[i].block, candidates[i].resolved = block, resolved
					if kind == "type" {
						candidates[i].kind = explainfiles.TypeReference
					}
				}
				block++
				kind, specifier, from = "", "", ""
			}
			previous = ""
			continue
		}
		previous = line
		missing, isMissing := missingFileTrace(line)
		if value, ok := traceField(line, "File name '", "' has a '.json' extension - stripping it."); kind == "module" && ok {
			jsonModule = value
		} else if !isMissing {
			jsonModule = ""
		}
		if strings.HasPrefix(line, "Loading module as file / folder") {
			m := candidateTrace(line)
			if kind == "" || m == nil {
				return "", nil, fmt.Errorf("unframed or malformed compiler candidate: %q", line)
			}
			if kind == "module" {
				candidate, err := relative(m[1])
				if err != nil {
					return "", nil, err
				}
				// This announcement precedes file probes; only a later skipped
				// directory establishes where a cold tree fits in their order.
				moduleCandidate = candidate
			}
		}
		if kind != "" {
			if isMissing {
				candidate, err := relative(missing)
				if err != nil {
					return "", nil, err
				}
				metadata := path.Base(candidate) == "package.json" && missing != jsonModule
				if firstParty(candidate) && (metadata || programCandidate(candidate) || kind == "module" && path.Ext(candidate) == ".json") {
					candidates = append(candidates, resolutionCandidate{from: from, specifier: specifier, path: candidate, file: !metadata, metadata: metadata})
				}
			}
			found, ok := foundPackageTrace(line)
			if !ok {
				found, ok = traceField(line, "File '", "' exists according to earlier cached lookups.")
			}
			if ok {
				candidate, err := relative(found)
				if err != nil {
					return "", nil, err
				}
				if firstParty(candidate) && path.Base(candidate) == "package.json" {
					candidates = append(candidates, resolutionCandidate{from: from, specifier: specifier, path: candidate, metadata: true})
				}
			}
			if missingDirectory, ok := traceField(line, "Directory '", "' does not exist, skipping all lookups in it."); kind == "module" && ok && moduleCandidate != "" {
				directory, err := relative(missingDirectory)
				if err != nil {
					return "", nil, err
				}
				if parentDir(moduleCandidate) == directory || moduleCandidate == directory {
					candidates = append(candidates, resolutionCandidate{from: from, specifier: specifier, path: moduleCandidate, skipped: directory})
				}
			}
		}
		if kind == "" {
			listing.WriteString(raw)
		}
	}
	if kind != "" {
		return "", nil, fmt.Errorf("unfinished compiler resolution trace for %q", specifier)
	}
	return listing.String(), candidates, nil
}
