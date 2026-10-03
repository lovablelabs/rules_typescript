// The strict-deps check: every edge tsgo lists from one of the target's own
// files has to land in a file the target or a direct dep owns.

package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// ownership is the manifest the rule writes beside the tsgo action: which
// label owns each file the program can resolve an edge to.
type ownership struct {
	label  string
	own    map[string]bool
	direct map[string]bool
	files  map[string][]fileOwner
}

type fileOwner struct {
	label  string
	direct bool
}

func readOwnership(name string) (*ownership, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	o, err := parseOwnership(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return o, nil
}

func parseOwnership(text string) (*ownership, error) {
	o := &ownership{
		own:    map[string]bool{},
		direct: map[string]bool{},
		files:  map[string][]fileOwner{},
	}
	for i, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "label" && len(f) == 2:
			o.label = f[1]
		case f[0] == "own" && len(f) == 2:
			o.own[f[1]] = true
		case f[0] == "direct" && len(f) == 2:
			o.direct[f[1]] = true
		case f[0] == "file" && len(f) == 3:
			o.files[f[2]] = append(o.files[f[2]], fileOwner{label: f[1]})
		case (f[0] == "npm-direct" && len(f) == 3) || (f[0] == "npm" && len(f) == 4):
			tree := f[len(f)-1]
			if path.IsAbs(tree) || path.Clean(tree) != tree || !strings.Contains(tree, "/.pnpm/") || npmPackageRoot(tree) != tree {
				return nil, fmt.Errorf("ownership manifest line %d needs an exact npm store File path; use tsaction from the same rules_typescript source", i+1)
			}
			owner := fileOwner{direct: f[0] == "npm-direct"}
			if !owner.direct {
				owner.label = f[2]
			}
			o.files[tree] = append(o.files[tree], owner)
		default:
			return nil, fmt.Errorf("ownership manifest line %d: %q", i+1, line)
		}
	}
	return o, nil
}

type finding struct {
	from, verb, specifier, to, label string
}

// check returns one finding per edge from an own file into a file whose owner
// is not a direct dep; a file nothing owns is an error, not a finding.
func (o *ownership) check(l *explainfiles.Listing) ([]finding, error) {
	seen := map[finding]bool{}
	var out []finding
	for _, e := range l.Edges {
		if !o.own[e.From] {
			continue
		}
		label, declared, err := o.owner(e.To)
		if err != nil {
			return nil, fmt.Errorf("%s %s %s %w", e.From, verb(e.Kind), quoted(e), err)
		}
		if declared {
			continue
		}
		f := finding{e.From, verb(e.Kind), quoted(e), e.To, label}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.from != b.from {
			return a.from < b.from
		}
		if a.specifier != b.specifier {
			return a.specifier < b.specifier
		}
		return a.to < b.to
	})
	return out, nil
}

func verb(k explainfiles.EdgeKind) string {
	switch k {
	case explainfiles.Reference:
		return "references"
	case explainfiles.TypeReference:
		return "references types"
	case explainfiles.Augmentation:
		return "augments"
	}
	return "imports"
}

// quoted spells the specifier as the source does: a module specifier in
// double quotes, a directive's attribute in single.
func quoted(e explainfiles.Edge) string {
	if e.Kind.ModuleSpecifier() {
		return `"` + e.Specifier + `"`
	}
	return "'" + e.Specifier + "'"
}

// owner names the label owning `to` and whether this target may use it: an
// own src, a store tree a dep's link enters, or a first-party file or tree.
func (o *ownership) owner(to string) (label string, declared bool, err error) {
	if o.own[to] {
		return o.label, true, nil
	}
	for p := to; p != "." && p != "/" && p != ""; p = path.Dir(p) {
		owners, ok := o.files[p]
		if !ok {
			continue
		}
		for _, owner := range owners {
			if owner.direct || o.direct[owner.label] {
				return owner.label, true, nil
			}
		}
		return owners[0].label, false, nil
	}
	if underNodeModules(to) {
		return "", false, fmt.Errorf("resolves to %s, under a package the npm closure does not hold", to)
	}
	return "", false, fmt.Errorf("resolves to %s, which no src, dep or npm "+
		"package of this target owns", to)
}

// report is the failing action's message: each edge with the file it resolved
// to and the deps entry that owns it.
func (o *ownership) report(findings []finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s imports files no direct dep provides:\n", o.label)
	for _, f := range findings {
		fmt.Fprintf(&b, "  %s %s %s\n    resolved to %s\n    add %s to deps\n",
			f.from, f.verb, f.specifier, f.to, f.label)
	}
	b.WriteString("Each reaches this target only through another dep's own " +
		"deps. Run Gazelle,\nwhich writes deps from these edges, or add the " +
		"labels above by hand.\n")
	return b.String()
}

type undeclaredDeps struct{ report string }

func (e *undeclaredDeps) Error() string {
	return strings.TrimSuffix(e.report, "\n")
}
