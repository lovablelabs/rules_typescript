package typescript

// What the merger does to a hand value is docs/gazelle/directives.md's; this
// file only says so, since the merger does it in silence.

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

// Read off Kinds(): a second list of what Gazelle recomputes would go stale
// when a kind gains an attribute.
func managedAttrs(kind string) (mergeable, resolved []string) {
	info, ok := (&tsLang{}).Kinds()[kind]
	if !ok {
		return nil, nil
	}
	for attr := range info.MergeableAttrs {
		mergeable = append(mergeable, attr)
		if info.ResolveAttrs[attr] {
			resolved = append(resolved, attr)
		}
	}
	sort.Strings(mergeable)
	sort.Strings(resolved)
	return mergeable, resolved
}

// Before the merge, the one point where the value on disk and the recomputed
// one are both in hand.
func reportManagedAttrDrops(args language.GenerateArgs, gen []*rule.Rule, imports ...any) {
	if args.File == nil {
		return
	}
	for i, want := range gen {
		mergeable, resolved := managedAttrs(want.Kind())
		for _, have := range args.File.Rules {
			if have.Kind() != want.Kind() || have.Name() != want.Name() || have.ShouldKeep() {
				continue
			}
			for _, attr := range mergeable {
				if attrKept(have, attr) {
					continue
				}
				expr := have.Attr(attr)
				if expr == nil {
					continue
				}
				if !isLiteralAttrValue(expr) {
					reportUnmergeableExpr(args.File.Path, have, attr, expr)
					continue
				}
				if slices.Contains(resolved, attr) && attr != "srcs" {
					continue
				}
				dropped := droppedAttrValues(args, have, want, attr)
				if attr == "srcs" && len(dropped) > 0 && i < len(imports) {
					if imps, ok := imports[i].(*ruleImports); ok && imps != nil {
						imps.reportSrcDrops = func(final *rule.Rule) {
							retained := map[label.Label]bool{}
							for _, value := range final.AttrStrings("srcs") {
								if lbl, err := parseLabel(value); err == nil {
									retained[sourceLabelIdentity(lbl, args)] = true
								}
							}
							dropped = slices.DeleteFunc(dropped, func(value string) bool {
								lbl, err := parseLabel(value)
								return err == nil && retained[sourceLabelIdentity(lbl, args)]
							})
							reportDroppedValues(args, have, attr, dropped)
						}
						continue
					}
				}
				reportDroppedValues(args, have, attr, dropped)
			}
		}
	}
}

// The shapes a merge reconciles value by value, the two rule.MergeRules
// knows; anything else is replaced.
func isLiteralAttrValue(e bzl.Expr) bool {
	switch v := e.(type) {
	case nil:
		return true
	case *bzl.Ident:
		return v.Name == "True" || v.Name == "False"
	case *bzl.StringExpr:
		return true
	case *bzl.ListExpr:
		for _, el := range v.List {
			if _, ok := el.(*bzl.StringExpr); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// Which of the two the merger picks depends on the shape, and reproducing that
// decision here means reproducing rule/merge.go's internals: an Ident is
// replaced with its contents lost, a "list + list" is refused and left alone.
// Naming both outcomes is the part that stays true.
func reportUnmergeableExpr(path string, r *rule.Rule, attr string, expr bzl.Expr) {
	start, _ := expr.Span()
	log.Printf("typescript: %s:%d: %s(%s) declares %s as an expression Gazelle's merger cannot "+
		"reconcile value by value, so %s is no longer an attribute Gazelle maintains: it either "+
		"replaces the whole expression, losing what it computed, or leaves it untouched and stops "+
		"updating it. A \"# keep\" comment above the attribute makes that yours deliberately.",
		path, start.Line, r.Kind(), r.Name(), attr, attr)
}

// Mirrors rule.MergeRules, which is what actually removes them.
func droppedAttrValues(args language.GenerateArgs, have, want *rule.Rule, attr string) []string {
	if s := have.AttrString(attr); s != "" {
		if want.AttrString(attr) == s || !derivableValue(args, s) {
			return nil
		}
		return []string{s}
	}
	carried := map[string]struct{}{}
	for _, v := range want.AttrStrings(attr) {
		carried[v] = struct{}{}
	}
	var dropped []string
	for _, e := range listElements(have.Attr(attr)) {
		s, ok := e.(*bzl.StringExpr)
		if !ok || rule.ShouldKeep(e) {
			continue
		}
		if _, held := carried[s.Value]; held || !derivableValue(args, s.Value) {
			continue
		}
		dropped = append(dropped, s.Value)
	}
	return dropped
}

var fileExtension = regexp.MustCompile(`\.[A-Za-z0-9]{1,6}$`)

// A value naming a file or package that is no longer there was dropped because
// the tree changed, and telling the user to hold it with "# keep" is telling
// them to name a source nothing provides -- which fails analysis rather than
// surviving the run. Only a path-shaped value is checked: "app" is a mode, not
// a missing directory.
func derivableValue(args language.GenerateArgs, value string) bool {
	switch {
	case value == "", strings.HasPrefix(value, "@"), strings.HasPrefix(value, ":"):
		return true
	case strings.HasPrefix(value, "//"):
		pkg, _, _ := strings.Cut(strings.TrimPrefix(value, "//"), ":")
		return pkg == "" || pathExists(args.Config.RepoRoot, pkg)
	case strings.Contains(value, ":"):
		return true
	case strings.Contains(value, "/"), fileExtension.MatchString(value):
		return pathExists(args.Dir, value)
	}
	return true
}

func pathExists(base, rel string) bool {
	_, err := os.Stat(filepath.Join(base, filepath.FromSlash(rel)))
	return err == nil
}

func reportDroppedValues(args language.GenerateArgs, r *rule.Rule, attr string, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	quoted := make([]string, 0, len(dropped))
	for _, v := range dropped {
		quoted = append(quoted, `"`+v+`"`)
	}
	log.Printf("typescript: %s(%s) in %s: Gazelle generates %s and recomputed it from the tree, "+
		"so %s is no longer declared. A value Gazelle cannot derive needs a \"# keep\" comment on "+
		"its own line to survive the next run; \"# keep\" above the attribute hands the whole "+
		"attribute back to you.",
		r.Kind(), r.Name(), args.File.Path, attr, strings.Join(quoted, ", "))
}

func listElements(e bzl.Expr) []bzl.Expr {
	list, ok := e.(*bzl.ListExpr)
	if !ok {
		return nil
	}
	return list.List
}

// attrKept reports a "# keep" on the attribute, which the merger checks itself
// but the direct write path has to ask about.
func attrKept(r *rule.Rule, key string) bool {
	comments := r.AttrComments(key)
	if comments == nil {
		return false
	}
	for _, comment := range append(comments.Before, comments.Suffix...) {
		text := strings.TrimSpace(strings.TrimPrefix(comment.Token, "#"))
		if text == "keep" || strings.HasPrefix(text, "keep: ") {
			return true
		}
	}
	return false
}

func sourceLabelIdentity(lbl label.Label, args language.GenerateArgs) label.Label {
	return labelIdentity(lbl, args.Config.RepoName, args.Rel)
}

func labelIdentity(lbl label.Label, repo, pkg string) label.Label {
	lbl = lbl.Abs(repo, pkg)
	if lbl.Repo == "" || lbl.Repo == "@" {
		lbl.Repo = repo
		lbl.Canonical = false
	}
	return lbl
}

func relativeLabel(lbl label.Label, repo, pkg string) label.Label {
	lbl = labelIdentity(lbl, repo, pkg)
	if lbl.Canonical {
		return lbl
	}
	return lbl.Rel(repo, pkg)
}
