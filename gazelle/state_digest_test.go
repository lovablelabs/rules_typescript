package typescript

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

func digestFixtureRule() *rule.Rule {
	r := rule.NewRule("ts_library", "lib")
	r.SetAttr("srcs", []string{"a.ts", "b.ts"})
	r.SetAttr("deps", []string{"//app/sub", "//:node_modules/zod"})
	r.SetPrivateAttr("ts_kind", "library")
	r.SetPrivateAttr("ts_specs", map[resolve.ImportSpec]bool{{Lang: "typescript", Imp: "app/lib"}: true})
	return r
}

func digestFixtureListing() explainfiles.Listing {
	var l explainfiles.Listing
	for i := range 200 {
		from, to := fmt.Sprintf("src/f%03d.ts", i), fmt.Sprintf("src/f%03d.ts", i+1)
		l.Files = append(l.Files, from)
		l.Edges = append(l.Edges, explainfiles.Edge{Kind: explainfiles.Import, From: from, To: to, Specifier: "./" + to})
	}
	l.Roots = []string{"src/f000.ts"}
	return l
}

func digestFixtureProgram() *program {
	return &program{
		Listing:    digestFixtureListing(),
		candidates: []resolutionCandidate{{from: "src/f000.ts", specifier: "zod", path: "node_modules/zod/index.d.ts", file: true, resolved: "node_modules/zod/index.d.ts"}},
		resolution: resolutionFacts{Kinds: []string{"bundler"}},
		config:     "tsconfig.json",
		dir:        "app",
		key:        "listing-key",
	}
}

func digestFixtureEntry() *cachedListing {
	return &cachedListing{
		Listing:    digestFixtureListing(),
		Candidates: []cachedCandidate{{From: "src/f000.ts", Specifier: "zod", Path: "node_modules/zod/index.d.ts", File: true}},
		Resolution: resolutionFacts{Kinds: []string{"bundler"}},
	}
}

func itemDigests() []struct {
	name   string
	digest func() [sha256.Size]byte
	golden string
} {
	r, p, entry := digestFixtureRule(), digestFixtureProgram(), digestFixtureEntry()
	return []struct {
		name   string
		digest func() [sha256.Size]byte
		golden string
	}{
		{"rule body", func() [sha256.Size]byte { return digestRuleBody(r).sum }, "e5c20ebfacfe434cd7b07724436495d5e1fcf0ab02f9bfd424140bf822bf1584"},
		{"program", func() [sha256.Size]byte { return programDigest(p) }, "6bf73fa7144782d953f962d91c853f28d2386109c03671c1bab0c622fb2c3e38"},
		{"listing entry", func() [sha256.Size]byte { return listingEntryDigest(entry) }, "08e0dbf75ea113aeabb3984edb0ac5f3e6235ea4bb01263a818682be878129d1"},
	}
}

// Resolve records on disk are keyed by these bytes; a change orphans every one.
func TestStateDigest_ItemDigestsKeepTheirRecordedBytes(t *testing.T) {
	for _, c := range itemDigests() {
		t.Run(c.name, func(t *testing.T) {
			sum := c.digest()
			if have := hex.EncodeToString(sum[:]); have != c.golden {
				t.Errorf("digests to %s, records hold %s", have, c.golden)
			}
		})
	}
}

// A replay digests every rule, program and listing of the run; a 64 KiB
// write buffer per item made that most of a replay's allocation.
func TestStateDigest_AnItemDigestAllocatesNoLargeWriteBuffer(t *testing.T) {
	const limit = 16 << 10
	for _, c := range itemDigests() {
		t.Run(c.name, func(t *testing.T) {
			if have := allocatedPerCall(func() { c.digest() }); have > limit {
				t.Errorf("one digest allocates %d bytes, over %d", have, limit)
			}
		})
	}
}

// allocatedPerCall counts bytes, not allocations as testing.AllocsPerRun does:
// a large buffer is one allocation.
func allocatedPerCall(f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const n = 100
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range n {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / n
}

type namedLanguage string

func (n namedLanguage) Name() string { return string(n) }

// Gazelle 2 tags generated rules with _gazelle_lang and _gazelle_kind; any
// other private attribute of those types must still refuse to record.
func TestStateDigest_Gazelle2PrivateAttributesDigestOnlyUnderTheirOwnKeys(t *testing.T) {
	body := func(key string, value any) ruleBody {
		r := rule.NewRule("ts_compile", "app")
		r.SetPrivateAttr(key, value)
		return digestRuleBody(r)
	}
	for _, tc := range []struct {
		key   string
		value any
	}{{"_gazelle_lang", namedLanguage("typescript")}, {"_gazelle_kind", rule.KindInfo{}}} {
		if got := body(tc.key, tc.value).opaque; got != "" {
			t.Errorf("%s refused to record: %s", tc.key, got)
		}
	}
	if body("_gazelle_lang", namedLanguage("typescript")).sum == body("_gazelle_lang", namedLanguage("proto")).sum {
		t.Error("the language a rule's _gazelle_lang names is not digested")
	}
	for _, value := range []any{namedLanguage("typescript"), rule.KindInfo{}} {
		if body("_other", value).opaque == "" {
			t.Errorf("a %T under another private attribute was digested", value)
		}
	}
	if gazelleDefaults("defaults", namedLanguage("defaults")) {
		t.Error("a defaults config from another package was taken for language/defaults")
	}
}
