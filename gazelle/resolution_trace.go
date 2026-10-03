package typescript

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

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

var moduleTraceStart = regexp.MustCompile(`^======== Resolving module '(.*)' from '(.*)'\. ========$`)
var moduleTraceEnd = regexp.MustCompile(`^======== Module name '(.*)' (?:was not resolved\.|was successfully resolved to '(.*?)'(?: with Package ID '.*?')?\.) ========$`)
var typeTraceStart = regexp.MustCompile(`^======== Resolving type reference directive '(.*?)', .* ========$`)
var typeTraceEnd = regexp.MustCompile(`^======== Type reference directive '(.*?)' (?:was not resolved\.|was successfully resolved to '(.*?)'(?: with Package ID '.*?')?, primary: .*?\.) ========$`)
var typeContainingFileTrace = regexp.MustCompile(`containing file '(.*?)'`)
var importsTrace = regexp.MustCompile(`^Using 'imports' subpath '.*' with target '(.*)'\.$`)
var candidateTrace = regexp.MustCompile(`^Loading module as file / folder, candidate module location '(.*)', target file types: (.+)\.$`)
var jsonModuleTrace = regexp.MustCompile(`^File name '(.*)' has a '\.json' extension - stripping it\.$`)
var missingFileTrace = regexp.MustCompile(`^File '(.*)' does not exist(?: according to earlier cached lookups)?\.$`)
var foundPackageTrace = regexp.MustCompile(`^Found 'package.json' at '(.*)'\.$`)
var cachedPackageTrace = regexp.MustCompile(`^File '(.*)' exists according to earlier cached lookups\.$`)
var missingDirectoryTrace = regexp.MustCompile(`^Directory '(.*)' does not exist, skipping all lookups in it\.$`)

// Trace arrays are emitted serially by the compiler's filesparser, before its listing.
func splitResolutionTrace(root, output string) (string, []resolutionCandidate, error) {
	var listing strings.Builder
	var candidates []resolutionCandidate
	var kind, specifier, from, previous, jsonModule, moduleCandidate string
	block, blockStart := 0, 0
	relative := func(value string) (string, error) {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("compiler resolution path is not absolute: %q", value)
		}
		rel, err := filepath.Rel(root, value)
		return filepath.ToSlash(rel), err
	}
	for _, raw := range strings.SplitAfter(output, "\n") {
		line := strings.TrimRight(raw, "\r\n")
		if strings.HasPrefix(line, "========") {
			switch {
			case moduleTraceStart.MatchString(line), typeTraceStart.MatchString(line):
				if kind != "" {
					start := moduleTraceStart.FindStringSubmatch(line)
					redirect := importsTrace.FindStringSubmatch(previous)
					if kind != "module" || start == nil || redirect == nil || start[1] != redirect[1] || !filepath.IsAbs(start[2]) {
						return "", nil, fmt.Errorf("unrecognised nested compiler resolution trace: %q", line)
					}
					previous = ""
					continue
				}
				moduleCandidate = ""
				blockStart = len(candidates)
				if m := moduleTraceStart.FindStringSubmatch(line); m != nil {
					kind, specifier = "module", m[1]
					var err error
					from, err = relative(m[2])
					if err != nil {
						return "", nil, err
					}
				} else {
					kind, specifier = "type", typeTraceStart.FindStringSubmatch(line)[1]
					if m := typeContainingFileTrace.FindStringSubmatch(line); m != nil {
						var err error
						from, err = relative(m[1])
						if err != nil {
							return "", nil, err
						}
					}
				}
			default:
				pattern := moduleTraceEnd
				if kind == "type" {
					pattern = typeTraceEnd
				}
				m := pattern.FindStringSubmatch(line)
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
		if m := jsonModuleTrace.FindStringSubmatch(line); kind == "module" && m != nil {
			jsonModule = m[1]
		} else if !missingFileTrace.MatchString(line) {
			jsonModule = ""
		}
		if strings.HasPrefix(line, "Loading module as file / folder") {
			m := candidateTrace.FindStringSubmatch(line)
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
			if m := missingFileTrace.FindStringSubmatch(line); m != nil {
				candidate, err := relative(m[1])
				if err != nil {
					return "", nil, err
				}
				metadata := path.Base(candidate) == "package.json" && m[1] != jsonModule
				if firstParty(candidate) && (metadata || programCandidate(candidate) || kind == "module" && path.Ext(candidate) == ".json") {
					candidates = append(candidates, resolutionCandidate{from: from, specifier: specifier, path: candidate, file: !metadata, metadata: metadata})
				}
			}
			m := foundPackageTrace.FindStringSubmatch(line)
			if m == nil {
				m = cachedPackageTrace.FindStringSubmatch(line)
			}
			if m != nil {
				candidate, err := relative(m[1])
				if err != nil {
					return "", nil, err
				}
				if firstParty(candidate) && path.Base(candidate) == "package.json" {
					candidates = append(candidates, resolutionCandidate{from: from, specifier: specifier, path: candidate, metadata: true})
				}
			}
			if m := missingDirectoryTrace.FindStringSubmatch(line); kind == "module" && m != nil && moduleCandidate != "" {
				directory, err := relative(m[1])
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
