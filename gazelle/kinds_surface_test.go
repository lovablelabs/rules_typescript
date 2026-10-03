package typescript

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/rule"
)

// wantKinds is every kind this extension writes or withdraws.
var wantKinds = []string{
	"exports_files",
	"filegroup",
	"node_modules",
	"node_modules_member",
	"npm_virtual_store",
	"ts_codegen",
	"ts_compile",
	"ts_config",
	"ts_dev_server",
	"ts_proto_config",
	"ts_proto_library",
	"ts_test",
}

var wantLoads = map[string][]string{
	"@npm//:defs.bzl":                   {"npm_virtual_store"},
	"@rules_typescript//npm:defs.bzl":   {"node_modules", "node_modules_member"},
	"@rules_typescript//proto:defs.bzl": {"ts_proto_config", "ts_proto_library"},
	"@rules_typescript//ts:defs.bzl": {
		"ts_codegen", "ts_compile", "ts_config", "ts_dev_server", "ts_test",
	},
}

// A goneKind back in Kinds() or a load would have Gazelle write a rule it must
// not: six that no .bzl defines, and the two that are written by hand.
var goneKinds = []string{
	"next_build", "next_dev_server", "sveltekit_build", "ts_bundle",
	"vite_bundler", "ts_lint",
	"ts_add_package", "ts_pnpm",
}

func TestKinds_ExactSurface(t *testing.T) {
	kinds := (&tsLang{}).Kinds()
	got := make([]string, 0, len(kinds))
	for kind := range kinds {
		got = append(got, kind)
	}
	sort.Strings(got)
	if !slices.Equal(got, wantKinds) {
		t.Errorf("Kinds() = %v\nwant %v", got, wantKinds)
	}
	for _, kind := range goneKinds {
		if _, ok := kinds[kind]; ok {
			t.Errorf("Kinds() still knows %q", kind)
		}
	}
}

// The loads, each file's symbols exactly wantLoads' in that order: a symbol
// Kinds() lacks or a gone kind fails here, and so does a map-ordered list.
func TestLoads_NamingEveryKind(t *testing.T) {
	lang := &tsLang{}
	for name, loads := range map[string][]rule.LoadInfo{
		"Loads":         lang.Loads(),
		"ApparentLoads": lang.ApparentLoads(func(string) string { return "" }),
	} {
		got := map[string][]string{}
		for _, li := range loads {
			got[li.Name] = li.Symbols
		}
		if !reflect.DeepEqual(got, wantLoads) {
			t.Errorf("%s = %v\nwant %v", name, got, wantLoads)
		}
	}
	apparent := lang.ApparentLoads(func(module string) string {
		return strings.ReplaceAll(module, "rules_typescript", "rules_ts~")
	})
	var names []string
	for _, li := range apparent {
		names = append(names, li.Name)
	}
	want := []string{"@npm//:defs.bzl", "@rules_ts~//npm:defs.bzl",
		"@rules_ts~//proto:defs.bzl", "@rules_ts~//ts:defs.bzl"}
	if !slices.Equal(names, want) {
		t.Errorf("ApparentLoads under an apparent name loads %v, want %v",
			names, want)
	}
}
