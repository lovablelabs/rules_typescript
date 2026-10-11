package runtimeview

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func npmRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, store := range []string{"_main/store/a", "_main/store/b"} {
		if err := os.MkdirAll(filepath.Join(root, store), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "_main/store/a"), filepath.Join(root, "_main/store/alias")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolveNpmBindingsSharesOneMapPerSetSoPlacementDedupesByIdentity(t *testing.T) {
	zero, one := 0, 1
	sets := []map[string]string{{"pkg": "_main/store/a"}, {"pkg": "_main/store/b"}}
	contexts := []NpmContext{
		{Module: "_main/web/x.ts", BindingsSet: &zero, PackageScope: &NpmPackageScope{Manifest: "_main/web/package.json", BindingsSet: &zero}},
		{Module: "_main/web/y.ts", BindingsSet: &zero},
		{Module: "_main/web/z.ts", BindingsSet: &one},
	}
	if err := ResolveNpmBindings(contexts, sets); err != nil {
		t.Fatal(err)
	}
	identity := func(m map[string]string) uintptr { return reflect.ValueOf(m).Pointer() }
	if identity(contexts[0].Bindings) != identity(sets[0]) || identity(contexts[1].Bindings) != identity(sets[0]) || identity(contexts[0].PackageScope.Bindings) != identity(sets[0]) {
		t.Fatal("contexts naming one bindings set do not share its map")
	}
	if identity(contexts[2].Bindings) == identity(sets[0]) {
		t.Fatal("contexts naming different bindings sets share a map")
	}
}

func TestPlaceNpmContextsSharedBindingSetLinksEachDirectoryOnce(t *testing.T) {
	root := npmRoot(t)
	shared := map[string]string{"pkg": "_main/store/a"}
	modules := []string{"_main/web/x.ts", "_main/web/y.ts", "_main/web/sub/z.ts"}
	scope := &NpmPackageScope{Manifest: "_main/web/package.json", Bindings: shared}
	contexts := []NpmContext{
		{Module: modules[0], Source: "x", Bindings: shared, PackageScope: scope},
		{Module: modules[1], Source: "y", Bindings: shared, PackageScope: scope},
		{Module: modules[2], Source: "z", Bindings: shared, PackageScope: scope},
	}
	if err := PlaceNpmContexts(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	sameAuthority(t, filepath.Join(root, "_main/web/node_modules/pkg"), filepath.Join(root, "_main/store/a"), true)
	sameAuthority(t, filepath.Join(root, "_main/web/sub/node_modules/pkg"), filepath.Join(root, "_main/store/a"), true)
}

func TestPlaceNpmContextsConflictingSetsInOneDirectoryNameTheFirstContext(t *testing.T) {
	root := npmRoot(t)
	setA := map[string]string{"pkg": "_main/store/a"}
	setB := map[string]string{"pkg": "_main/store/b"}
	modules := []string{"_main/web/x.ts", "_main/web/y.ts"}
	contexts := []NpmContext{
		{Module: modules[0], Source: "x", Bindings: setA},
		{Module: modules[0], Source: "x2", Bindings: setA},
		{Module: modules[1], Source: "y", Bindings: setB},
	}
	err := PlaceNpmContexts(root, contexts, modules)
	if err == nil || !strings.Contains(err.Error(), `source "y" npm "pkg" from store "_main/store/b"`) || !strings.Contains(err.Error(), `source "x" selecting store "_main/store/a"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlaceNpmContextsAliasedStoreForSameDestinationIsNotAConflict(t *testing.T) {
	root := npmRoot(t)
	modules := []string{"_main/web/x.ts", "_main/web/y.ts"}
	contexts := []NpmContext{
		{Module: modules[0], Source: "x", Bindings: map[string]string{"pkg": "_main/store/a"}},
		{Module: modules[1], Source: "y", Bindings: map[string]string{"pkg": "_main/store/alias"}},
	}
	if err := PlaceNpmContexts(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	sameAuthority(t, filepath.Join(root, "_main/web/node_modules/pkg"), filepath.Join(root, "_main/store/a"), true)
}

func TestPlaceNpmContextsRejectsUnadmittedModuleAndNonNormalizedAnchorOnEveryLookup(t *testing.T) {
	root := npmRoot(t)
	shared := map[string]string{"pkg": "_main/store/a"}
	modules := []string{"_main/web/x.ts"}
	if err := PlaceNpmContexts(root, []NpmContext{{Module: "_main/web/other.ts", Source: "o", Bindings: shared}}, modules); err == nil || !strings.Contains(err.Error(), "no admitted runtime module") {
		t.Fatalf("unadmitted module: err = %v", err)
	}
	contexts := []NpmContext{
		{Module: modules[0], Source: "x", Bindings: shared},
		// Its directory is the first lookup's, with the same map: only the anchor check rejects it.
		{Module: modules[0], Source: "x2", Bindings: shared, PackageScope: &NpmPackageScope{Manifest: "_main/web/./package.json", Bindings: shared}},
	}
	if err := PlaceNpmContexts(root, contexts, modules); err == nil || !strings.Contains(err.Error(), "non-normalized npm context path") {
		t.Fatalf("non-normalized scope manifest already seen: err = %v", err)
	}
}
