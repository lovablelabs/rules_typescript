package runtimeview

import (
	"os"
	"path/filepath"
	"testing"
)

func walkUpRoot(t *testing.T, modules ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"_main/store/pkg", "_main/store/other", "_main/node_modules/@scope"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"_main/node_modules/pkg": "../store/pkg", "_main/node_modules/@scope/pkg": "../../store/pkg"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, module := range modules {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, module)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, module), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireSelects(t *testing.T, root, link, store string) {
	t.Helper()
	got, err := os.Stat(filepath.Join(root, link))
	want, wantErr := os.Stat(filepath.Join(root, store))
	if err != nil || wantErr != nil || !os.SameFile(got, want) {
		t.Fatalf("%s does not select %s: %v, %v", link, store, err, wantErr)
	}
}

func requireNoLink(t *testing.T, root, link string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, link)); !os.IsNotExist(err) {
		t.Fatalf("%s was linked although the walk-up already selects its store: %v", link, err)
	}
}

func TestPlaceNpmContextsSkippingResolvedKeepsNearestStoreOnEveryWalkUp(t *testing.T) {
	modules := []string{"_main/app/main.js", "_main/app/sub/main.js", "_main/app/sub/deep/main.js", "_main/app/scoped.js"}
	root := walkUpRoot(t, modules...)
	contexts := []NpmContext{
		{Module: modules[0], Source: "a.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}},
		{Module: modules[1], Source: "b.ts", Bindings: map[string]string{"pkg": "_main/store/other"}},
		{Module: modules[2], Source: "c.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}},
		{Module: modules[3], Source: "d.ts", Bindings: map[string]string{"@scope/pkg": "_main/store/pkg"}},
	}
	if err := PlaceNpmContextsSkippingResolved(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	requireNoLink(t, root, "_main/app/node_modules/pkg")
	requireNoLink(t, root, "_main/app/node_modules/@scope/pkg")
	requireSelects(t, root, "_main/app/sub/node_modules/pkg", "_main/store/other")
	requireSelects(t, root, "_main/app/sub/deep/node_modules/pkg", "_main/store/pkg")
}

func TestPlaceNpmContextsSkippingResolvedSkipsBelowALinkItPlacedForTheSameStore(t *testing.T) {
	modules := []string{"_main/app/main.js", "_main/app/sub/main.js"}
	root := walkUpRoot(t, modules...)
	contexts := []NpmContext{
		{Module: modules[0], Source: "a.ts", Bindings: map[string]string{"pkg": "_main/store/other"}},
		{Module: modules[1], Source: "b.ts", Bindings: map[string]string{"pkg": "_main/store/other"}},
	}
	if err := PlaceNpmContextsSkippingResolved(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	requireSelects(t, root, "_main/app/node_modules/pkg", "_main/store/other")
	requireNoLink(t, root, "_main/app/sub/node_modules/pkg")
}

func TestPlaceNpmContextsWithoutSkippingStillLinksEveryFreeDestination(t *testing.T) {
	modules := []string{"_main/app/main.js"}
	root := walkUpRoot(t, modules...)
	contexts := []NpmContext{{Module: modules[0], Source: "a.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}}}
	if err := PlaceNpmContexts(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	requireSelects(t, root, "_main/app/node_modules/pkg", "_main/store/pkg")
}

func TestPlaceNpmContextsSkippingResolvedWalksPastNodeModulesWithoutTheName(t *testing.T) {
	modules := []string{"_main/app/sub/deep/main.js"}
	root := walkUpRoot(t, modules...)
	for _, dir := range []string{"_main/app/node_modules/unrelated", "_main/app/sub/node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contexts := []NpmContext{{Module: modules[0], Source: "a.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}}}
	if err := PlaceNpmContextsSkippingResolved(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	requireNoLink(t, root, "_main/app/sub/deep/node_modules/pkg")
}

func TestPlaceNpmContextsSkippingResolvedLinksBelowANodeModulesSegment(t *testing.T) {
	// ESM and vite do not skip node_modules segments on their walk, CJS does.
	modules := []string{"_main/app/node_modules/x/main.js"}
	root := walkUpRoot(t, modules...)
	contexts := []NpmContext{{Module: modules[0], Source: "a.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}}}
	if err := PlaceNpmContextsSkippingResolved(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	requireSelects(t, root, "_main/app/node_modules/x/node_modules/pkg", "_main/store/pkg")
}

func TestPlaceNpmContextsSkippingResolvedLinksWhenAnExtensionFileShadowsTheWalk(t *testing.T) {
	// CJS require("pkg") takes node_modules/pkg.js before the store further up.
	modules := []string{"_main/app/sub/main.js"}
	root := walkUpRoot(t, modules...)
	if err := os.MkdirAll(filepath.Join(root, "_main/app/node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "_main/app/node_modules/pkg.js"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	contexts := []NpmContext{{Module: modules[0], Source: "a.ts", Bindings: map[string]string{"pkg": "_main/store/pkg"}}}
	if err := PlaceNpmContextsSkippingResolved(root, contexts, modules); err != nil {
		t.Fatal(err)
	}
	requireSelects(t, root, "_main/app/sub/node_modules/pkg", "_main/store/pkg")
}

func TestPlaceNpmContextsSeesALinkItPlacedThroughADirectoryAlias(t *testing.T) {
	// _main/top/node_modules aliases b/node_modules, which only this pass creates.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"_main/store/p1", "_main/store/p2", "_main/empty", "_main/node_modules", "_main/top/a", "_main/top/b", "_main/top/c"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"_main/node_modules/n":     "../store/p1",
		"_main/top/node_modules":   "b/node_modules",
		"_main/top/a/node_modules": "../../empty",
		"_main/top/c/node_modules": "../../empty",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	modules := []string{"_main/top/a/f.js", "_main/top/b/f.js", "_main/top/c/f.js"}
	contexts := []NpmContext{
		{Module: modules[0], Source: "a", Bindings: map[string]string{"n": "_main/store/p1"}},
		{Module: modules[1], Source: "b", Bindings: map[string]string{"n": "_main/store/p2"}},
		{Module: modules[2], Source: "c", Bindings: map[string]string{"n": "_main/store/p1"}},
	}
	if err := PlaceNpmContexts(root, contexts, modules); err == nil {
		t.Fatal("c's walk up reaches b's p2 link through the alias, yet no conflict was raised")
	}
}
