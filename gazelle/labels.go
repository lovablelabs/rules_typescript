package typescript

// A srcs entry is a label, not a path. Bazel reads the head of the string
// rather than the file system, so a file whose name happens to start the way a
// label prefix does names something else -- or nothing.

import (
	"log"
	"strings"
	"sync"

	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
)

type parsedLabel struct {
	label label.Label
	err   error
}

var parsedLabels = struct {
	sync.Mutex
	m map[string]parsedLabel
}{m: map[string]parsedLabel{}}

// label.Parse runs regular expressions, and ownership queries reparse the same
// labels for every rule of every ancestor package.
func parseLabel(text string) (label.Label, error) {
	parsedLabels.Lock()
	defer parsedLabels.Unlock()
	parsed, ok := parsedLabels.m[text]
	if !ok {
		parsed.label, parsed.err = label.Parse(text)
		parsedLabels.m[text] = parsed
	}
	return parsed.label, parsed.err
}

// srcLabel is the label that names a file the generated package holds, and
// whether Bazel can name it at all.
//
// A bare name is already that label for almost every file. The exceptions are
// the label grammar's two prefixes: "@" opens a repository name and "//" an
// absolute package. "@{$username}.tsx" -- a TanStack route on a dynamic
// segment -- therefore reads as repository "{$username}.tsx" and fails
// `bazel query //...` for the whole workspace with "invalid repository name". A
// leading ":" pins the name to the package being generated, which is what a
// bare name means everywhere else.
//
// A ":" anywhere is the one name no spelling reaches: it splits package from
// target, and a target name may not contain one. Bare, Bazel reads a package
// that is not there; pinned, it refuses with "target names may not contain
// ':'". Nothing else is rejected here -- a name Bazel merely dislikes is still
// a name it can be told about, and dropping a source Bazel would have accepted
// is worse than letting Bazel object to it.
func srcLabel(name string) (string, bool) {
	if strings.Contains(name, ":") {
		return "", false
	}
	if strings.HasPrefix(name, "@") || strings.HasPrefix(name, "//") {
		return ":" + name, true
	}
	return name, true
}

// srcLabels is srcLabel over a list, dropping a name that has no label: an
// entry Bazel cannot parse aborts every query and every build over the
// workspace, not only the target that declares it.
func srcLabels(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if lbl, ok := srcLabel(name); ok {
			out = append(out, lbl)
		}
	}
	return out
}

// Said where every regular file passes through, once per file per run, since
// dropping a source in silence is the defect keep.go exists to remove.
func reportUnlabelableFile(args language.GenerateArgs, name string) {
	log.Printf("typescript: %s holds %q, a name no Bazel label can spell -- a target name may "+
		"not contain \":\" -- so no generated target names it and nothing compiles it. Rename "+
		"the file.", orRepoRoot(args.Rel), name)
}

func orRepoRoot(rel string) string {
	if rel == "" {
		return "the workspace root"
	}
	return rel
}
