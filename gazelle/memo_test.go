package typescript

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// recordTask runs task as a task of s that -ts_memo=record keeps, and returns its entry.
func recordTask(s *programStore, task func()) taskEntry {
	if s.recorder == nil {
		s.recorder = newTaskRecorder(false)
	}
	s.verified("completion", "//lib", nil, task)
	s.recorder.settle()
	return s.recorder.entries[len(s.recorder.entries)-1]
}

// spelled is each question's oracle, arguments and the answer the run gave them last, in order, and an owner fold
// as its digest: what a test compares where the entry keeps ids and digests.
func spelled(r *taskRecorder, questions []question) []string {
	var out []string
	for _, q := range questions {
		if len(q.args) == 0 {
			out = append(out, fmt.Sprintf("%s %x", q.oracle, q.sum))
		}
		for _, id := range q.args {
			out = append(out, q.oracle+strings.ReplaceAll(r.questions.args[id], "\x00", " ")+" = "+r.questions.pairs[r.questions.latest[id]].answer)
		}
	}
	slices.Sort(out)
	return out
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ReadDigestsMoveWithTheValuesATaskReadAndNothingElse(t *testing.T) {
	read := func(s *programStore) []readDigest {
		return recordTask(s, func() {
			s.semanticRule("//lib")
			s.buildFile("lib")
			s.observedOutputs("gen")
			s.storedHoldings("")
			s.protoProviderIndex()
		}).reads
	}
	before := read(readsetStore(t))
	for _, c := range []struct {
		name, want string
		write      func(s *programStore)
	}{
		{name: "a store rebuilt from the same BUILD files, with other stamps and rule objects"},
		{name: "a read rule edited in place", want: "rule //lib", write: func(s *programStore) {
			s.emission.rules["//lib"].SetAttr("srcs", []string{"m.ts"})
		}},
		{name: "a read directory declares another output", want: "dir gen", write: func(s *programStore) { replaceGen(s, "x.ts", "y.ts") }},
		{name: "a rule leaves a read BUILD file", want: "file lib", write: func(s *programStore) { s.emission.files["lib"].Rules = nil }},
		{name: "a read directory's producer is renamed under the same outputs", want: "dir gen, keys ", write: func(s *programStore) {
			producer := rule.NewRule("genrule", "other")
			producer.SetAttr("outs", []string{"x.ts"})
			s.emission.files["gen"].Rules[0] = producer
			s.emission.setRule("//gen:other", producer)
		}},
		{name: "a read BUILD file's text changes under the same rules", want: "file lib", write: func(s *programStore) {
			s.emission.files["lib"].Content = []byte("# edited\n")
		}},
		{name: "a read BUILD file's text changes to other text of the same length", want: "file lib", write: func(s *programStore) {
			content := bytes.Clone(s.emission.files["lib"].Content)
			content[0] ^= 1
			s.emission.files["lib"].Content = content
		}},
		{name: "an unread rule changes", write: func(s *programStore) {
			s.emission.rules["//app"].SetAttr("srcs", []string{"b.ts"})
		}},
		{name: "a key joins the store", want: "keys ", write: func(s *programStore) {
			s.inputs["new"] = s.inputs["lib"]
			s.emission.setRule("//new", rule.NewRule("ts_compile", "new"))
		}},
		{name: "a proto wrapper joins the store", want: "keys , proto ", write: func(s *programStore) {
			getConfig(s.inputs["lib"].config).protos = newProtoStore()
			wrapper := rule.NewRule("ts_proto_library", "w")
			wrapper.SetAttr("out_dir", "gen")
			wrapper.SetAttr("proto", ":p")
			s.emission.setRule("//lib:w", wrapper)
		}},
		{name: "a rule becomes a proto wrapper", want: "dir gen, proto ", write: func(s *programStore) {
			getConfig(s.inputs["gen"].config).protos = newProtoStore()
			wrapper := rule.NewRule("ts_proto_library", "gen")
			wrapper.SetAttr("out_dir", "gen")
			wrapper.SetAttr("proto", ":p")
			s.emission.setRule("//gen", wrapper)
		}},
		{name: "a proto library joins the store", want: "keys , proto ", write: func(s *programStore) {
			getConfig(s.inputs["lib"].config).protos = newProtoStore()
			p := rule.NewRule("proto_library", "p")
			p.SetAttr("srcs", []string{"a.proto"})
			s.emission.setRule("//lib:p", p)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := readsetStore(t)
			if c.write != nil {
				c.write(s)
			}
			after := read(s)
			var moved []string
			for i, got := range after {
				if got != before[i] {
					moved = append(moved, got.category+" "+got.name)
				}
			}
			if len(after) != len(before) || strings.Join(moved, ", ") != c.want {
				t.Fatalf("digests moved for %q, want %q", moved, c.want)
			}
		})
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_EachOracleLogsItsQuestionOncePerArgumentsWithItsAnswer(t *testing.T) {
	root, lock := npmRepo(t)
	e := importEdge("web/a.ts", "react", store+"react/18.0.0/abc/node_modules/react/index.js")
	manifest := fixtureManifest(root, "web")
	for _, c := range []struct {
		oracle string
		// ask puts the question, spelled one way or an other, and returns the answer it was given.
		ask func(s *programStore, other bool) []string
	}{
		{"lock.edgeLabel", func(_ *programStore, other bool) []string {
			return []string{lock.edgeLabel(e, map[bool]string{false: "web", true: "app"}[other], manifest)}
		}},
		{"lock.memberView", func(_ *programStore, other bool) []string {
			view, ok := lock.memberView(map[bool]string{false: "@acme/lib", true: "@acme/lib/wire"}[other], "web/a.ts", "web", manifest)
			return []string{view, strconv.FormatBool(ok)}
		}},
		{"lock.memberLabel", func(_ *programStore, other bool) []string {
			return []string{lock.memberLabel("@acme/lib", map[bool]string{false: "web", true: "app"}[other], "web")}
		}},
		{"lock.runtimePackage", func(_ *programStore, other bool) []string {
			name, linked := lock.runtimePackage(e, manifest, other)
			return []string{name, strconv.FormatBool(linked)}
		}},
		{"lock.edgeImporter", func(_ *programStore, other bool) []string {
			importer, required := lock.edgeImporter(e, manifest, "web", other)
			return []string{importer, strconv.FormatBool(required)}
		}},
		{"lock.label", func(_ *programStore, other bool) []string {
			return []string{lock.label("react", map[bool]string{false: "web", true: "app"}[other])}
		}},
		{"lock.declaring", func(_ *programStore, other bool) []string {
			imp, ok := lock.declaring("react", map[bool]string{false: "web", true: "app"}[other])
			return []string{imp, strconv.FormatBool(ok)}
		}},
		{"lock.linksTo", func(_ *programStore, other bool) []string {
			return []string{strconv.FormatBool(lock.linksTo(map[bool]string{false: "packages/lib", true: "web"}[other]))}
		}},
		{"lock.isMember", func(_ *programStore, other bool) []string {
			return []string{strconv.FormatBool(lock.isMember(map[bool]string{false: "@acme/lib", true: "react"}[other]))}
		}},
		{"lock.memberCompilerTarget", func(_ *programStore, other bool) []string {
			return []string{lock.memberCompilerTarget("", map[bool]string{false: "@npm//:acme_lib", true: "@npm//:other"}[other])}
		}},
		{"lock.workersPool", func(_ *programStore, other bool) []string {
			return []string{workersPoolResolution(lock, "//web:node_modules", label.New("", map[bool]string{false: "web", true: "app"}[other], "x"))}
		}},
		{"manifest", func(s *programStore, other bool) []string {
			return []string{manifestKey(s.nearestManifest(s.inputs["app"].config, map[bool]string{false: "app", true: "lib"}[other]))}
		}},
		{"sourceFile", func(s *programStore, other bool) []string {
			s.files["app"] = []string{"a.ts"}
			return []string{strconv.FormatBool(s.sourceFile(map[bool]string{false: "app/a.ts", true: "app/b.ts"}[other]))}
		}},
		{"wranglerConfig", func(s *programStore, other bool) []string {
			s.wranglerConfigs = map[string]string{"app/a.mts": "app/wrangler.jsonc", "app/b.mts": ""}
			file, _ := s.wranglerConfigOf(s.inputs["app"].config, map[bool]string{false: "app/a.mts", true: "app/b.mts"}[other])
			return []string{file}
		}},
		{"vitest program", func(s *programStore, other bool) []string {
			s.vitestPrograms = map[string]*program{"app/a.mts": {dir: "app"}, "app/b.mts": {dir: "app", config: "app/b.mts"}}
			return []string{s.recorder.programSum(s.configProgram("", map[bool]string{false: "app/a.mts", true: "app/b.mts"}[other]))}
		}},
		{"config", func(s *programStore, other bool) []string {
			pkg := map[bool]string{false: "lib", true: "app"}[other]
			s.semanticRule("//" + pkg)
			sum := s.recorder.configSum(s.inputs[pkg].config)
			return []string{string(sum[:])}
		}},
		{"config", func(s *programStore, other bool) []string {
			pkg := map[bool]string{false: "lib", true: "app"}[other]
			s.storedInputFiles(pkg, s.emission.rules["//"+pkg])
			sum := s.recorder.configSum(s.inputs[pkg].config)
			return []string{string(sum[:])}
		}},
		{"bazelPackage", func(s *programStore, other bool) []string {
			return []string{strconv.FormatBool(s.bazelPackage(map[bool]string{false: "app", true: "nowhere"}[other]))}
		}},
	} {
		t.Run(c.oracle, func(t *testing.T) {
			s := readsetStore(t)
			var answer []string
			answers := func(entry taskEntry) (answers []string) {
				for _, q := range entry.questions {
					if q.oracle == c.oracle {
						for _, id := range q.args {
							answers = append(answers, s.recorder.questions.pairs[s.recorder.questions.latest[id]].answer)
						}
					}
				}
				return answers
			}
			entry := recordTask(s, func() { answer = c.ask(s, false); c.ask(s, false) })
			if got := answers(entry); len(got) != 1 || got[0] != strings.Join(answer, "\x00") {
				t.Fatalf("%s logged %v for the same arguments, want one question with the answer %q", c.oracle, spelled(s.recorder, entry.questions), answer)
			}
			if n := len(answers(recordTask(s, func() { c.ask(s, false); c.ask(s, true) }))); n != 2 {
				t.Fatalf("%s logged %d questions for two argument lists, want 2", c.oracle, n)
			}
		})
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ATaskThatConfiguresAndStartsFromEachInputRecordsItsInputsDigest(t *testing.T) {
	base := func() (*rule.Rule, *rule.Rule, *ruleImports, label.Label) {
		return rule.NewRule("ts_compile", "app"), rule.NewRule("ts_compile", "app"),
			&ruleImports{sources: []string{"app/a.ts"}, deps: []string{"//lib"}, srcDrops: &srcDrops{have: rule.NewRule("ts_compile", "app")}},
			label.New("", "app", "app")
	}
	digest := func(change func(r, effective *rule.Rule, imps *ruleImports, from *label.Label)) [32]byte {
		s := readsetStore(t)
		r, effective, imps, from := base()
		if change != nil {
			change(r, effective, imps, &from)
		}
		return recordTask(s, func() { noteInputs(s.inputs["app"].config, r, effective, imps, from) }).inputs
	}
	want := digest(nil)
	for name, change := range map[string]func(r, effective *rule.Rule, imps *ruleImports, from *label.Label){
		"the generated rule": func(r, _ *rule.Rule, _ *ruleImports, _ *label.Label) { r.SetAttr("srcs", []string{"a.ts"}) },
		"the merged rule":    func(_, e *rule.Rule, _ *ruleImports, _ *label.Label) { e.SetAttr("srcs", []string{"a.ts"}) },
		"the sources":        func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) { i.sources = []string{"app/b.ts"} },
		"the declared deps":  func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) { i.deps = nil },
		"the config file":    func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) { i.config = "app/vitest.config.mts" },
		"the edges": func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) {
			i.edges = []explainfiles.Edge{{From: "a", To: "b"}}
		},
		"the candidates": func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) {
			i.candidates = []resolutionCandidate{{from: "a"}}
		},
		"the program":         func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) { i.program = &program{dir: "app"} },
		"the dropped sources": func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) { i.srcDrops.dropped = []string{"x"} },
		"the rule they drop from": func(_, _ *rule.Rule, i *ruleImports, _ *label.Label) {
			i.srcDrops.have.SetAttr("srcs", []string{"a.ts"})
		},
		"the rule it is for": func(_, _ *rule.Rule, _ *ruleImports, from *label.Label) { *from = label.New("", "app", "other") },
	} {
		if got := digest(change); got == want {
			t.Errorf("the inputs digest does not move with %s", name)
		}
	}
	if digest(nil) != want {
		t.Error("the inputs digest of the same task differs between two stores")
	}
	s := readsetStore(t)
	s.importFacts = newImportFactsRecorder(nil)
	r, effective, imps, from := base()
	if recordTask(s, func() { noteInputs(s.inputs["app"].config, r, effective, imps, from) }).inputs == want {
		t.Error("the inputs digest ignores whether the run records import facts, which a task's facts outputs follow")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_IndexAndOverrideQuestionsKeepOneOrderAndFollowEachAnswer(t *testing.T) {
	c := emptyConfig()
	overridden := emptyConfig()
	directive, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve typescript gen/tree //x:y\n"))
	if err != nil {
		t.Fatal(err)
	}
	(&resolve.Configurer{}).Configure(overridden, "", directive)
	ix := buildIndex(t, c, indexedRule{kind: "ts_codegen", name: "indexed", pkg: "gen", outDir: "tree"})
	s := readsetStore(t)
	specs := []resolve.ImportSpec{{Lang: languageName, Imp: "gen/tree"}, {Lang: languageName, Imp: "gen/other"}}
	lookups := func(do func()) []question { return recordTask(s, do).questions }
	asked := recordTask(s, func() { findRules(ix, nil, specs[0], languageName) }).questions
	if !slices.ContainsFunc(asked, func(q question) bool {
		return q.oracle == "index" && len(q.args) == 1 && s.recorder.questions.args[q.args[0]] == "\x00"+languageName+"\x00gen/tree\x00"+languageName
	}) {
		t.Fatalf("an index lookup is not a question with its arguments, so a replay cannot ask it again: %v", spelled(s.recorder, asked))
	}
	proto := resolve.ImportSpec{Lang: "proto", Imp: "a.proto"}
	oracles := func(questions []question) (names []string) {
		for _, q := range questions {
			names = append(names, q.oracle)
		}
		return names
	}
	if got := oracles(lookups(func() { findRules(ix, nil, proto, "proto"); findRules(ix, c, proto, "proto") })); !slices.Equal(got, []string{"index", "index (config)"}) {
		t.Errorf("lookups with and without a configuration asked %v, want two oracles: only the configured one asks the cross-resolvers", got)
	}
	inOrder := lookups(func() {
		findRules(ix, nil, specs[0], languageName)
		findRules(ix, nil, specs[1], languageName)
		findOverride(c, specs[0], languageName)
	})
	if got := lookups(func() {
		findOverride(c, specs[0], languageName)
		findRules(ix, nil, specs[1], languageName)
		findRules(ix, nil, specs[0], languageName)
	}); !sameQuestions(got, inOrder) {
		t.Error("the same lookups in another order gave other questions")
	}
	for name, do := range map[string]func(){
		"another rule answering": func() {
			findRules(indexOf(languageName, specs[0], "gen:other"), nil, specs[0], languageName)
			findRules(ix, nil, specs[1], languageName)
			findOverride(c, specs[0], languageName)
		},
		"another index answer": func() {
			findRules(buildIndex(t, c), nil, specs[0], languageName)
			findRules(ix, nil, specs[1], languageName)
			findOverride(c, specs[0], languageName)
		},
		"another language asked": func() {
			findRules(ix, nil, specs[0], "proto")
			findRules(ix, nil, specs[1], languageName)
			findOverride(c, specs[0], languageName)
		},
		"another spec language": func() {
			findRules(ix, nil, resolve.ImportSpec{Lang: "proto", Imp: specs[0].Imp}, languageName)
			findRules(ix, nil, specs[1], languageName)
			findOverride(c, specs[0], languageName)
		},
		"another override answer": func() {
			findRules(ix, nil, specs[0], languageName)
			findRules(ix, nil, specs[1], languageName)
			findOverride(overridden, specs[0], languageName)
		},
		"no override lookup": func() {
			findRules(ix, nil, specs[0], languageName)
			findRules(ix, nil, specs[1], languageName)
		},
	} {
		if got := lookups(do); sameQuestions(got, inOrder) {
			t.Errorf("%s: the questions did not move", name)
		}
	}
}

func TestMemo_LookupFieldsThatRunTogetherFoldApartAndARepeatedLookupCounts(t *testing.T) {
	if newAskKey("o", []string{"ab", "c"}).joined() == newAskKey("o", []string{"a", "bc"}).joined() ||
		questionSum("o", "\x00a", "b") == questionSum("o", "\x00ab", "") {
		t.Error("two lookups whose fields run together digest alike")
	}
	var x, y lookupFold
	x.add(questionSum("o", "", "same"))
	x.add(questionSum("o", "", "same"))
	y.add(questionSum("o", "", "other"))
	y.add(questionSum("o", "", "other"))
	if x.sum() == y.sum() {
		t.Error("two answers each given twice fold alike, so a changed answer to a repeated lookup reads as clean")
	}
}

func TestMemo_ARulesKeepCommentAndArgumentsAreInItsDigest(t *testing.T) {
	plain := rule.NewRule("ts_compile", "a")
	kept := rule.NewRule("ts_compile", "a")
	kept.AddComment("# keep")
	args := rule.NewRule("ts_compile", "a")
	args.AddArg(&bzl.StringExpr{Value: "x"})
	if ruleDigest(plain) == ruleDigest(kept) || ruleDigest(plain) == ruleDigest(args) {
		t.Error("a rule's digest ignores its comment or its positional arguments")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_AStoredEntrysCheckOrANestedTasksEntryAddsNoReadToTheTaskItRunsInside(t *testing.T) {
	s := readsetStore(t)
	file := &storeFile{path: filepath.Join(t.TempDir(), "memo"), key: "k", binary: "b"}
	entry := taskEntry{kind: "completion", key: "//lib", reads: []readDigest{{"dir", "gen", [32]byte{1}}}}
	if err := os.WriteFile(file.path, encodeMemoStore("k", "b", questionArgs{}, []taskEntry{entry}), 0o644); err != nil {
		t.Fatal(err)
	}
	s.recorder = newTaskRecorder(true)
	s.recorder.store = file
	file.startLoad()
	parent := recordTask(s, func() { s.recorder.previous(s, "completion", "//lib") })
	if len(parent.reads) != 0 {
		t.Errorf("checking a stored entry made reads of the task it ran inside: %v", parent.reads)
	}
	parent = recordTask(s, func() { s.verified("completion", "//app", nil, func() { s.storedHoldings("") }) })
	if len(parent.reads) != 0 {
		t.Errorf("making a nested task's entry made reads of the task it ran inside: %v", parent.reads)
	}
}

func TestMemo_ADirectorysOutputsDigestMovesWithWhatItsDeclarationsLeaveOpen(t *testing.T) {
	outputs := func(change func(o *packageOutputs)) [32]byte {
		o := &packageOutputs{incomplete: -1, trees: []outputTree{{root: "gen/t", rule: 0}}}
		change(o)
		return outputsDigest(o)
	}
	base := outputs(func(*packageOutputs) {})
	for name, change := range map[string]func(o *packageOutputs){
		"a rule whose outputs are not literal": func(o *packageOutputs) { o.incomplete = 0 },
		"another tree root":                    func(o *packageOutputs) { o.trees[0].root = "gen/u" },
		"another rule declaring the tree":      func(o *packageOutputs) { o.trees[0].rule = 1 },
	} {
		if outputs(change) == base {
			t.Errorf("the outputs digest ignores %s", name)
		}
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_AnOwnerCallAsksWhetherTheOwnerIsPendingAndAReachWhetherTheCompletionIsRooted(t *testing.T) {
	questions := func(pending, rooted bool, task func(cx *programCompletion)) []question {
		s := readsetStore(t)
		if pending {
			s.completePrograms["//lib"] = func(*programCompletion) {}
		}
		recordTask(s, func() { task(&programCompletion{s: s, rooted: rooted}) })
		return s.recorder.entries[len(s.recorder.entries)-1].questions
	}
	owner := func(cx *programCompletion) { cx.ownerMode("//lib") }
	if sameQuestions(questions(true, true, owner), questions(false, true, owner)) {
		t.Error("an owner call does not ask whether the owner was pending")
	}
	// A rooted completion's reached labels are outputs an unrooted one does not have.
	reach := func(cx *programCompletion) { cx.reach("//dep") }
	if sameQuestions(questions(false, true, reach), questions(false, false, reach)) {
		t.Error("a completion that reaches a dep does not ask whether it is rooted")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_EachReadOfWalkStateMovesTheQuestionsWithTheState(t *testing.T) {
	file := filepath.Join(t.TempDir(), "present.ts")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent.ts")
	for _, c := range []struct {
		name         string
		state, other func(s *programStore)
		read         func(s *programStore)
	}{
		{"the names the walk saw", func(s *programStore) { s.files["app"] = []string{"a.ts"} }, func(s *programStore) { s.files["app"] = []string{"b.ts"} },
			func(s *programStore) { s.dirNames("app") }},
		{"whether the walk generated a directory", func(s *programStore) { s.walked["app"] = true }, func(s *programStore) { s.walked["app"] = false },
			func(s *programStore) { s.dirWalked("app") }},
		{"whether a package is nearest", func(s *programStore) { s.packages["app"] = map[string]bool{} }, func(*programStore) {},
			func(s *programStore) { s.nearestPackage("app/sub") }},
		{"the package a source belongs to", func(s *programStore) {
			s.files["app"] = []string{"a.ts"}
			s.walked["app"] = true
			s.packages["app"] = map[string]bool{"app/a.ts": true}
		}, func(s *programStore) {
			s.files["app"] = []string{"a.ts"}
			s.walked["app"] = true
			s.packages["app"] = map[string]bool{}
		}, func(s *programStore) { s.owner("app/a.ts") }},
		{"the package that lists a source", func(s *programStore) {
			s.files["app"] = []string{"a.ts"}
			s.walked["app"] = true
			s.packages["app"] = map[string]bool{}
		}, func(s *programStore) {
			s.files["app"] = []string{"a.ts"}
			s.walked["app"] = true
		}, func(s *programStore) { s.whyUnowned("app/a.ts") }},
		{"whether a package is generated", func(s *programStore) { s.generatedPackages["lib"] = true }, func(s *programStore) { s.generatedPackages["lib"] = false },
			func(s *programStore) { (&programCompletion{s: s, rooted: true}).ownerMode("//lib") }},
		{"a foreign manifest", func(s *programStore) { s.foreign["app"] = "app/package.json" }, func(*programStore) {},
			func(s *programStore) { s.foreignAt("app") }},
		{"a regular file", func(*programStore) {}, func(s *programStore) { s.regularFiles = map[string]bool{file: false} },
			func(s *programStore) { s.regularFile(file) }},
		{"a file that is absent", func(*programStore) {}, func(s *programStore) { s.regularFiles = map[string]bool{missing: true} },
			func(s *programStore) { s.regularFile(missing) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			lookups := func(prepare func(s *programStore)) string {
				s := readsetStore(t)
				prepare(s)
				return fmt.Sprint(recordTask(s, func() { c.read(s) }).questions)
			}
			if lookups(c.state) == lookups(c.other) {
				t.Fatalf("the questions did not move with %s", c.name)
			}
		})
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ANearestPackageLookupKeepsTheDirectoryItWasAskedForWhereItStopsAtTheSameAnswer(t *testing.T) {
	lookups := func(dir string) string {
		s := readsetStore(t)
		s.packages["app"] = map[string]bool{}
		return fmt.Sprint(recordTask(s, func() { s.nearestPackage(dir) }).questions)
	}
	if lookups("app/sub") == lookups("app/other/deep") {
		t.Error("two directories under one package fold alike, so a moved package boundary below the answer reads as clean")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_AFirstPassThatLeavesItsRuleForACompletionAsksForTheProgramAndSaysSo(t *testing.T) {
	entry := func(hasProgram bool) taskEntry {
		s := readsetStore(t)
		s.installChecked = true
		if hasProgram {
			in := s.inputs["app"]
			in.program = &program{dir: "app"}
			s.inputs["app"] = in
		}
		r := rule.NewRule("ts_compile", "app")
		return recordTask(s, func() {
			resolveRuleEdges(s.inputs["app"].config, buildIndex(t, emptyConfig()), r, r, &ruleImports{}, label.New("", "app", "app"), nil)
		})
	}
	without, with := entry(false), entry(true)
	if sameQuestions(without.questions, with.questions) {
		t.Error("whether the package has a program is not a question, so a package that gains one reads as clean")
	}
	if slices.Equal(without.out, with.out) {
		t.Error("whether the rule is left for a completion is not an output, so a replay could not register the completion")
	}
}

// indexAnswer indexes each rule under its specs for lang.
type indexAnswer struct {
	lang  string
	specs []resolve.ImportSpec
}

func (a indexAnswer) Name() string { return a.lang }
func (a indexAnswer) Imports(*config.Config, *rule.Rule, *rule.File) []resolve.ImportSpec {
	return a.specs
}
func (indexAnswer) Embeds(*rule.Rule, label.Label) []label.Label { return nil }
func (indexAnswer) Resolve(*config.Config, *resolve.RuleIndex, *repo.RemoteCache, *rule.Rule, any, label.Label) {
}

// indexOf answers spec for lang with the rules at labels, each "pkg:name"; an empty one is no rule.
func indexOf(lang string, spec resolve.ImportSpec, labels ...string) *resolve.RuleIndex {
	ix := resolve.NewRuleIndex(func(*rule.Rule, string) resolve.Resolver { return indexAnswer{lang, []resolve.ImportSpec{spec}} })
	for _, l := range labels {
		if pkg, name, found := strings.Cut(l, ":"); found {
			ix.AddRule(config.New(), rule.NewRule("x", name), rule.EmptyFile("BUILD.bazel", pkg))
		}
	}
	ix.Finish()
	return ix
}

// Each row changes one oracle answer a task acts on, with every other read and question held. Not parallel: a
// recorded task captures the process-wide log.
func TestMemo_EachOracleCallSiteMovesTheEntryWhenItsAnswerChanges(t *testing.T) {
	ts := func(imp string) resolve.ImportSpec { return resolve.ImportSpec{Lang: languageName, Imp: imp} }
	pick := func(other bool, a, b string) string { return map[bool]string{false: a, true: b}[other] }
	for _, c := range []struct {
		name string
		call func(t *testing.T, s *programStore, other bool)
	}{
		{"the index's answer to ruleHolding", func(_ *testing.T, s *programStore, other bool) {
			s.semanticRule("//gen")
			ruleHolding(s.inputs["app"].config, indexOf(languageName, ts("gen/x.ts"), pick(other, "", "gen:gen")), ts("gen/x.ts"), label.New("", "app", "app"), sourceRole)
		}},
		{"the index's answer for a codegen tree", func(_ *testing.T, s *programStore, other bool) {
			s.indexedTree(indexOf(languageName, ts(codegenTreeKey("gen/tree")), pick(other, "", "gen:gen")), "gen/tree")
		}},
		{"the index's answer for a proto wrapper", func(_ *testing.T, s *programStore, other bool) {
			c := s.inputs["lib"].config
			getConfig(c).protos = newProtoStore()
			native := absoluteProtoLabel(c, label.New("", "lib", "p"), "")
			protoWrapperProviders(c, indexOf(languageName, ts(protoWrapperKey("lib/gen", native)), pick(other, "", "lib:w")), "lib/gen", native)
		}},
		{"the index's answer for a proto library another source also names", func(_ *testing.T, s *programStore, other bool) {
			c := s.inputs["lib"].config
			seedResolveConfig(c)
			protos := newProtoStore()
			protos.observations = map[string][]protoObservation{"lib": {{native: label.New("", "lib", "p"), paths: []string{"lib/a.proto"}}}}
			getConfig(c).protos = protos
			protoProvider(c, indexOf("proto", resolve.ImportSpec{Lang: "proto", Imp: "lib/a.proto"}, pick(other, "", "lib:p")), "lib/gen", "lib/a.proto")
		}},
		{"an owner's selected imports", func(_ *testing.T, s *programStore, other bool) {
			s.emission.rules["//app"].SetPrivateAttr("_ts_selected_imports", map[resolve.ImportSpec]bool{ts("lib/l.ts"): true})
			s.emission.rules["//lib"].SetPrivateAttr("_ts_selected_imports", map[resolve.ImportSpec]bool{ts(pick(other, "lib/l.ts", "lib/m.ts")): true})
			ruleHolding(s.inputs["gen"].config, indexOf(languageName, ts("lib/l.ts"), "lib:lib", "app:app"), ts("lib/l.ts"), label.New("", "gen", "gen"), sourceRole)
		}},
		{"a source export's visibility", func(t *testing.T, s *programStore, other bool) {
			src := "ts_compile(name = \"lib\", srcs = [\"l.ts\"])\nexports_files([\"l.ts\"], visibility = [\"//visibility:" + pick(other, "private", "public") + "\"])\n"
			f, err := rule.LoadData("lib/BUILD.bazel", "lib", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			s.recordBuild(s.repoConfig, "lib", f)
			s.files["lib"] = []string{"l.ts"}
			s.walked["lib"] = true
			s.exportedTo(s.inputs["app"].config, "lib/l.ts", label.New("", "app", "app"))
		}},
		{"whether the lockfile links the member", func(t *testing.T, s *programStore, other bool) {
			_, lock := npmRepo(t)
			if !other {
				lock = &npmLock{names: map[string]bool{}, importers: map[string]*pnpmImporter{}, members: map[string]string{}}
			}
			c := s.inputs["lib"].config
			getConfig(c).lock = lock
			s.installChecked = true
			resolveEdges(c, indexOf(languageName, ts("x")), rule.NewRule("ts_compile", "lib"), &ruleImports{}, label.New("", "packages/lib", "lib"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry := func(other bool) taskEntry {
				s := readsetStore(t)
				return recordTask(s, func() { c.call(t, s, other) })
			}
			a, b := entry(false), entry(true)
			if a.inputs == b.inputs && slices.Equal(a.reads, b.reads) && sameQuestions(a.questions, b.questions) {
				t.Fatalf("the entry does not move with %s, so a task acting on another answer reads as clean", c.name)
			}
		})
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_AnOraclesLookupsAreOneQuestionEachMemberMovesAndAReplayOfTheStoredArgumentsRecomputes(t *testing.T) {
	s := readsetStore(t)
	entry := func(answers map[string]string) taskEntry {
		return recordTask(s, func() {
			for _, arg := range slices.Sorted(maps.Keys(answers)) {
				ask("probe", []string{arg}, answers[arg])
			}
		})
	}
	base := entry(map[string]string{"a": "1", "b": "2", "c": "3"})
	if len(base.questions) != 1 || len(base.questions[0].args) != 3 {
		t.Fatalf("three lookups of one oracle are %v, want one question over the three", spelled(s.recorder, base.questions))
	}
	for name, answers := range map[string]map[string]string{
		"a member answered otherwise":      {"a": "1", "b": "9", "c": "3"},
		"a member asked with another file": {"a": "1", "b": "2", "d": "3"},
		"a member left out":                {"a": "1", "b": "2"},
		"a member added":                   {"a": "1", "b": "2", "c": "3", "d": "4"},
	} {
		if sameQuestions(entry(answers).questions, base.questions) {
			t.Errorf("%s: the question did not move, so a task that acted on it reads as clean", name)
		}
	}
	path := filepath.Join(t.TempDir(), "memo")
	if err := os.WriteFile(path, encodeMemoStore("k", "b", s.recorder.questions.questionArgs, []taskEntry{base}), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openMemoStore(path, "k", "b")
	if store == nil {
		t.Fatal("the store does not open")
	}
	stored, ok := store.entry(taskID{base.kind, base.key})
	if !ok || len(stored.questions) != 1 {
		t.Fatalf("stored entry %+v (%v), want its one question", stored, ok)
	}
	q := stored.questions[0]
	for i, id := range q.args {
		if store.args[id] != s.recorder.questions.args[base.questions[0].args[i]] {
			t.Fatalf("stored argument %d is %q, want the run's %q", i, store.args[id], s.recorder.questions.args[base.questions[0].args[i]])
		}
	}
	var fold lookupFold
	for _, id := range q.args {
		fold.add(questionSum(q.oracle, store.args[id], map[string]string{"\x00a": "1", "\x00b": "2", "\x00c": "3"}[store.args[id]]))
	}
	if groupSum(q.oracle, len(q.args), fold.sum()) != q.sum {
		t.Error("asking the stored arguments again with the same answers does not give the stored digest, so no replay can check it")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ADirectorysNamesAskedBeforeALoadListsItAreAskedAgainAfter(t *testing.T) {
	s := readsetStore(t)
	recordTask(s, func() { s.dirNames("late") })
	s.files["late"] = []string{"a.ts"}
	after := recordTask(s, func() { s.dirNames("late") })
	fresh := readsetStore(t)
	fresh.files["late"] = []string{"a.ts"}
	if want := recordTask(fresh, func() { fresh.dirNames("late") }); !sameQuestions(after.questions, want.questions) {
		t.Errorf("a task after the load asked %v, want %v: the names digest from before it stuck", spelled(s.recorder, after.questions), spelled(fresh.recorder, want.questions))
	}
	within := recordTask(readsetStore(t), func() {
		s.recorder = activeRun.recorder
		delete(s.files, "late")
		s.dirNames("late")
		s.files["late"] = []string{"a.ts"}
		s.dirNames("late")
	})
	if within.never == "" {
		t.Error("a task that saw a directory's names change kept no reason not to be stored")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_AskedQuestionsKeepOneOrderWhateverOrderTheTaskAskedThem(t *testing.T) {
	asked := func(first, second string) []question {
		s := readsetStore(t)
		return recordTask(s, func() {
			ask("probe", []string{first}, "x")
			ask("probe", []string{second}, "x")
		}).questions
	}
	if !sameQuestions(asked("a", "b"), asked("b", "a")) {
		t.Error("a task that asked its questions in another order has other questions, so equal state reads as stale")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ANestedCompletionsLogLinesAreItsOwnAndNotItsParents(t *testing.T) {
	s := readsetStore(t)
	s.completePrograms["//lib"] = func(*programCompletion) { log.Print("nested says") }
	var seen bytes.Buffer
	outer := log.Writer()
	log.SetOutput(&seen)
	parent := recordTask(s, func() {
		log.Print("before")
		(&programCompletion{s: s, rooted: true}).ownerMode("//lib")
		log.Print("after")
	})
	log.SetOutput(outer)
	if !strings.Contains(seen.String(), "nested says") {
		t.Error("the nested completion's log line did not reach the log")
	}
	nested := s.recorder.entries[0]
	has := func(e taskEntry, line string) bool {
		return slices.ContainsFunc(e.out, func(o output) bool { return o.kind == logOutput && strings.Contains(o.text, line) })
	}
	if !has(nested, "nested says") || has(parent, "nested says") {
		t.Errorf("the nested log line is in the nested outputs (%v) and the parent's (%v)", has(nested, "nested says"), has(parent, "nested says"))
	}
	if !has(parent, "before") || !has(parent, "after") {
		t.Error("the parent lost the lines it logged around its nested completion")
	}
}

func TestMemo_ConfigDigestIgnoresTheLockfileButNotItsAbsence(t *testing.T) {
	_, lock := npmRepo(t)
	digestOf := func(change func(c *config.Config)) [32]byte {
		s := readsetStore(t)
		c := s.inputs["app"].config
		if change != nil {
			change(c)
		}
		for _, q := range recordTask(s, func() {
			noteInputs(c, rule.NewRule("ts_compile", "app"), rule.NewRule("ts_compile", "app"), &ruleImports{}, label.New("", "app", "app"))
		}).questions {
			if q.oracle == "config" {
				return q.sum
			}
		}
		t.Fatal("no config question")
		return [32]byte{}
	}
	base := digestOf(nil)
	if got := digestOf(func(c *config.Config) { getConfig(c).lock = lock }); got == base {
		t.Error("a package with a lockfile and one without have one configuration digest")
	}
	other := &npmLock{names: map[string]bool{"x": true}, importers: map[string]*pnpmImporter{}, members: map[string]string{}}
	if digestOf(func(c *config.Config) { getConfig(c).lock = lock }) != digestOf(func(c *config.Config) { getConfig(c).lock = other }) {
		t.Error("the lockfile's content moved the configuration digest, which the lock questions cover")
	}
	if got := digestOf(func(c *config.Config) { getConfig(c).updateOnly = true }); got == base {
		t.Error("the configuration digest ignores a directive")
	}
	proto := func(c *config.Config) { getConfig(c).protos = &protoStore{roots: []string{"x"}} }
	if digestOf(proto) == base {
		t.Error("the configuration digest ignores the proto store")
	}
	if got := digestOf(func(c *config.Config) { c.Repos = []*rule.Rule{rule.NewRule("http_archive", "x")} }); got == base {
		t.Error("the configuration digest ignores the repositories")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_DigestsMemoizedByKeyKeepDistinctInputsApart(t *testing.T) {
	s := readsetStore(t)
	rec := newTaskRecorder(false)
	g := s.emission
	lib, app := g.rules["//lib"], g.rules["//app"]
	app.SetAttr("srcs", []string{"other.ts"})
	g.stamps["//app"] = g.stamps["//lib"]
	one, _ := rec.ruleSum(g, "//lib", lib)
	if two, _ := rec.ruleSum(g, "//lib", app); one == two {
		t.Error("two rules stored under one key and stamp share a cached digest")
	}
	if rec.namesSum("app", []string{"a.ts"}) == rec.namesSum("lib", []string{"b.ts"}) {
		t.Error("two directories share a names digest")
	}
	configs := func() [2]string {
		var sums [2]string
		for i, only := range []bool{false, true} {
			c := s.inputs["app"].config.Clone()
			getConfig(c).updateOnly = only
			sum := rec.configSum(c)
			sums[i] = string(sum[:])
		}
		return sums
	}
	if got := configs(); got[0] == got[1] {
		t.Error("two package configurations share a digest")
	}
	other := readsetStore(t)
	asked := spelled(other.recorder, recordTask(other, func() {
		s.recorder = activeRun.recorder
		s.noteSelected("lib", rule.NewRule("ts_compile", "a"), map[resolve.ImportSpec]bool{{Lang: languageName, Imp: "x"}: true})
		s.noteSelected("lib", rule.NewRule("ts_compile", "b"), map[resolve.ImportSpec]bool{{Lang: languageName, Imp: "y"}: true})
	}).questions)
	if len(asked) != 2 || strings.SplitN(asked[0], " = ", 2)[1] == strings.SplitN(asked[1], " = ", 2)[1] {
		t.Errorf("two owners' selected imports share a question or an answer: %v", asked)
	}
}

func TestMemo_FirstDifferenceSeesAnOutputOnlyOneSideHas(t *testing.T) {
	a := output{kind: logOutput, text: "a"}
	if _, _, differs := firstDifference([]output{a}, []output{a, a}); !differs {
		t.Error("an extra trailing output is not a difference")
	}
	if _, _, differs := firstDifference([]output{a, a}, []output{a}); !differs {
		t.Error("a missing trailing output is not a difference")
	}
	if _, _, differs := firstDifference([]output{a}, []output{a}); differs {
		t.Error("equal outputs differ")
	}
}

func TestMemo_SelectedImportsAreAskedOncePerOwnerWithTheirSetAsTheAnswer(t *testing.T) {
	owner := rule.NewRule("ts_compile", "lib")
	imports := func(imp string) map[resolve.ImportSpec]bool {
		return map[resolve.ImportSpec]bool{{Lang: languageName, Imp: imp}: true}
	}
	asked := func(imp string, twice bool) []question {
		s := readsetStore(t)
		return recordTask(s, func() {
			s.noteSelected("lib", owner, imports(imp))
			if twice {
				s.noteSelected("lib", owner, imports(imp))
			}
		}).questions
	}
	once, again, other := asked("lib/a.ts", false), asked("lib/a.ts", true), asked("lib/b.ts", false)
	if len(once) != 1 || len(once[0].args) != 1 || !sameQuestions(once, again) || sameQuestions(once, other) {
		t.Errorf("selected imports questions %v, %v, %v; want one per owner that follows the set", once, again, other)
	}
	s := readsetStore(t)
	before := recordTask(s, func() { s.noteSelected("lib", owner, imports("lib/a.ts")) }).questions
	if after := recordTask(s, func() { s.noteSelected("lib", owner, imports("lib/b.ts")) }).questions; sameQuestions(before, after) {
		t.Error("an owner whose imports were selected again later in the run kept the first answer")
	}
	unselected := map[resolve.ImportSpec]bool{{Lang: languageName, Imp: "lib/a.ts"}: false}
	if sameQuestions(once, recordTask(readsetStore(t), func() { s.noteSelected("lib", owner, unselected) }).questions) {
		t.Error("a selected import mapped to false asks what one mapped to true does, though ruleHolding reads the value")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ARuleOrConfigurationADigestCannotReadKeepsTheTaskOutOfTheStore(t *testing.T) {
	s := readsetStore(t)
	s.emission.rules["//lib"].SetPrivateAttr("_ts_imports", &ruleImports{})
	if got := recordTask(s, func() { s.semanticRule("//lib") }).never; got != "opaque rule" {
		t.Errorf("a task that read a rule pointing at shared imports: never %q, want opaque rule", got)
	}
	s = readsetStore(t)
	c := s.inputs["app"].config
	c.Exts["java"] = struct{ A int }{1}
	got := recordTask(s, func() {
		noteInputs(c, rule.NewRule("ts_compile", "app"), rule.NewRule("ts_compile", "app"), &ruleImports{}, label.New("", "app", "app"))
	}).never
	if got != "opaque config" {
		t.Errorf("a task in a package another language configures: never %q, want opaque config", got)
	}
	s = readsetStore(t)
	shared := rule.NewRule("ts_compile", "app")
	shared.SetPrivateAttr("_ts_imports", &ruleImports{})
	got = recordTask(s, func() {
		noteInputs(s.inputs["app"].config, shared, rule.NewRule("ts_compile", "app"), &ruleImports{}, label.New("", "app", "app"))
	}).never
	if got != "opaque rule" {
		t.Errorf("a task that starts from a rule pointing at shared imports: never %q, want opaque rule", got)
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_ARecordedTaskRunsWithoutTheResolutionMemoAndLeavesNoneBehind(t *testing.T) {
	s := readsetStore(t)
	s.memo = &resolutionMemo{}
	warm := false
	recordTask(s, func() {
		warm = s.memo != nil
		s.memo = &resolutionMemo{}
	})
	if warm || s.memo != nil {
		t.Errorf("a memo was warm at the start of a recorded task (%v) or left after it (%v), where a hit hides the reads behind it", warm, s.memo != nil)
	}
}

func TestMemo_AManifestsDepsAreInTheKeyOfItsQuestion(t *testing.T) {
	if manifestKey(&manifest{dir: "a", name: "n"}) == manifestKey(&manifest{dir: "a", name: "n", deps: []string{"react"}}) {
		t.Error("two manifests that differ in their deps have one key")
	}
}

func TestMemo_RuleSumFollowsTheStampAndWritesCarryTheKind(t *testing.T) {
	s := readsetStore(t)
	rec := newTaskRecorder(false)
	g := s.emission
	before, _ := rec.ruleSum(g, "//lib", g.rules["//lib"])
	g.rules["//lib"].SetAttr("srcs", []string{"m.ts"})
	g.stamp("//lib")
	if after, _ := rec.ruleSum(g, "//lib", g.rules["//lib"]); after == before {
		t.Error("a rule edited in place under a new stamp kept its cached digest")
	}
	if slices.Equal(ruleWrites("//lib", rule.NewRule("ts_compile", "lib")), ruleWrites("//lib", rule.NewRule("ts_test", "lib"))) {
		t.Error("two rules of different kinds leave the same writes")
	}
}

// Not parallel: a recorded task captures the process-wide log and a deferred fatal is process-wide.
func TestMemo_ATaskThatDidWhatAReplayCannotRepeatIsNeverStored(t *testing.T) {
	t.Cleanup(func() {
		pendingFatal.Lock()
		pendingFatal.f = nil
		pendingFatal.Unlock()
	})
	for _, c := range []struct {
		name, want string
		task       func(s *programStore)
	}{
		{"a stored task", "", func(s *programStore) { s.semanticRule("//lib") }},
		{"a deferred fatal", "fatal", func(*programStore) { deferFatal(label.New("", "lib", "lib"), "lib/l.ts", "app/a.ts", "reported later") }},
		{"the install check", "install check", func(s *programStore) { s.requireInstall(t.TempDir()) }},
		{"a deferred fatal before the install check", "fatal", func(s *programStore) {
			deferFatal(label.New("", "lib", "lib"), "lib/l.ts", "app/a.ts", "reported later")
			s.requireInstall(t.TempDir())
		}},
		{"a BUILD file load", "load", func(s *programStore) {
			s.emission.files["extra"] = &rule.File{Pkg: "extra"}
			s.observeBuilds([]string{"extra"})
		}},
		{"the first lookup of a wrangler config", "first log", func(s *programStore) {
			s.wranglerConfigOf(s.inputs["app"].config, "app/vitest.config.mts")
		}},
		{"an oracle that moved between two questions", "moving oracle probe", func(*programStore) {
			ask("probe", []string{"x"}, "before")
			ask("probe", []string{"x"}, "after")
		}},
		{"an override that moved", "moving oracle override", func(s *programStore) { moveOverride(t, s) }},
		{"an override that moved before a deferred fatal", "moving oracle override", func(s *programStore) {
			moveOverride(t, s)
			deferFatal(label.New("", "lib", "lib"), "lib/l.ts", "app/a.ts", "reported later")
		}},
		{"a deferred fatal before an override moved", "fatal", func(s *programStore) {
			deferFatal(label.New("", "lib", "lib"), "lib/l.ts", "app/a.ts", "reported later")
			moveOverride(t, s)
		}},
		{"an override that moved before another oracle did", "moving oracle override", func(s *programStore) {
			moveOverride(t, s)
			ask("probe", []string{"x"}, "before")
			ask("probe", []string{"x"}, "after")
		}},
		{"overrides looked up under two configurations", "lookups under two configurations", func(s *programStore) {
			seedResolveConfig(s.inputs["app"].config)
			seedResolveConfig(s.inputs["lib"].config)
			spec := resolve.ImportSpec{Lang: languageName, Imp: "lib/l.ts"}
			findOverride(s.inputs["app"].config, spec, languageName)
			findOverride(s.inputs["lib"].config, resolve.ImportSpec{Lang: languageName, Imp: "lib/m.ts"}, languageName)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := readsetStore(t)
			if got := recordTask(s, func() { c.task(s) }).never; got != c.want {
				t.Fatalf("never stored because %q, want %q", got, c.want)
			}
		})
	}
}

// moveOverride looks up an override of app/a.ts before and after a resolve directive adds one.
func moveOverride(t *testing.T, s *programStore) {
	c := s.inputs["app"].config
	seedResolveConfig(c)
	spec := resolve.ImportSpec{Lang: languageName, Imp: "app/a.ts"}
	findOverride(c, spec, languageName)
	f, err := rule.LoadData("BUILD.bazel", "", []byte("# gazelle:resolve "+languageName+" app/a.ts //other\n"))
	if err != nil {
		t.Fatal(err)
	}
	(&resolve.Configurer{}).Configure(c, "", f)
	if _, ok := findOverride(c, spec, languageName); !ok {
		t.Fatal("the resolve directive added no override")
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_KeySetProtoAndBuildFileReadsAreDigestedAsFirstReadNotAfterTheTasksOwnWrites(t *testing.T) {
	read := func(s *programStore, write func()) []readDigest {
		return recordTask(s, func() {
			s.buildFile("lib")
			s.storedHoldings("")
			s.protoProviderIndex()
			write()
		}).reads
	}
	want := read(readsetStore(t), func() {})
	s := readsetStore(t)
	got := read(s, func() {
		getConfig(s.inputs["lib"].config).protos = newProtoStore()
		p := rule.NewRule("proto_library", "p")
		p.SetAttr("srcs", []string{"a.proto"})
		s.emission.setRule("//lib:p", p)
		s.emission.files["lib"].Content = []byte("# written\n")
	})
	if !slices.Equal(got, want) {
		t.Errorf("a task that stored a rule and rewrote a BUILD file after reading them recorded %v, want the digests it read %v", got, want)
	}
}

// Not parallel: a recorded task captures the process-wide log.
func TestMemo_CompletionOutputsKeepOwnerCallsAndPromotionsInOrderAndNestedCompletionsApart(t *testing.T) {
	s := readsetStore(t)
	s.completePrograms["//lib"] = func(cx *programCompletion) {
		noteOutput(func() []output { return []output{{kind: logOutput, text: "lib completed"}} })
		cx.reach("//lib:dep")
	}
	parent := recordTask(s, func() {
		(&programCompletion{s: s, repo: "", rooted: true}).ownerMode("//lib")
	})
	nested := s.recorder.entries[0]
	if want := []output{{kind: logOutput, text: "lib completed"}, {kind: reachedOutput, key: "//lib:dep"}}; !slices.Equal(nested.out, want) {
		t.Errorf("the nested completion's outputs = %v, want %v", nested.out, want)
	}
	want := []output{
		{kind: reachedOutput, key: "//lib"},
		{kind: writeOutput, key: "//lib", name: "emit", text: "True"},
		{kind: completedOutput, key: "//lib"},
		{kind: ownerOutput, key: "//lib", text: strconv.Itoa(int(emittedEmission))},
	}
	if !slices.Equal(parent.out, want) {
		t.Errorf("the owner call's outputs = %v, want %v", parent.out, want)
	}
}

// The ids a facts recorder gives files follow the order it saw them, which differs between runs.
func TestMemo_FactsFragmentSpellsFilesByNameNotByRunLocalId(t *testing.T) {
	fragment := func(first ...string) string {
		s := readsetStore(t)
		s.importFacts = newImportFactsRecorder(nil)
		s.importFacts.mu.Lock()
		for _, file := range first {
			s.importFacts.id(file)
		}
		s.importFacts.mu.Unlock()
		out := recordTask(s, func() {
			call := s.importFacts.beginResolve()
			walk := call.beginWalk(&program{key: "listing"}, []string{"app/a.ts", "app/b.ts"}, false, func(dep string) string { return dep })
			walk.outcome("app/b.ts", "//app:b", true, false)
			walk.outcome("app/a.ts", "//app:a", true, false)
			walk.end(map[string]bool{"app/a.ts": true, "app/b.ts": true}, nil)
			call.end()
			chosen := []candidateKeyText{{"app/b.ts", "./x", 0}, {"app/a.ts", "./y", 1}}
			if first[0] == "app/b.ts" {
				slices.Reverse(chosen)
			}
			s.importFacts.selection("listing", true, []string{"app/b.ts", "app/a.ts"}, chosen)
		}).out
		if len(out) != 2 || out[0].kind != factsOutput || out[1].kind != factsOutput {
			t.Fatalf("outputs %v, want a resolve and a selection fragment", out)
		}
		for _, o := range out {
			if !json.Valid([]byte(o.text)) {
				t.Fatalf("fragment %q is not JSON, so a replay cannot read its paths back", o.text)
			}
		}
		return out[0].text + "\n" + out[1].text
	}
	if a, b := fragment("app/a.ts", "app/b.ts"), fragment("app/b.ts", "app/a.ts"); a != b {
		t.Fatalf("fragments differ with the order the files were first seen:\n%s\n%s", a, b)
	}
}

// sampleArgs is the question table sampleEntries' argument ids point into.
var sampleArgs = questionArgs{oracles: []string{"manifest", "index"}, oracleOf: []uint32{0, 1, 1}, args: []string{"\x00lib", "\x00a", "\x00b"}}

// askedOnce is the question of a task that asked oracle once, with the question table's first argument list.
func askedOnce(oracle, args, answer string) question {
	var fold lookupFold
	fold.add(questionSum(oracle, args, answer))
	return question{oracle: oracle, args: []uint32{0}, sum: groupSum(oracle, 1, fold.sum())}
}

func sampleEntries() []taskEntry {
	return []taskEntry{
		{kind: "completion", key: "//lib", inputs: [32]byte{1},
			reads:     []readDigest{{"rule", "//lib", [32]byte{2}}, {"keys", "", [32]byte{3}}},
			questions: []question{{"manifest", []uint32{0}, [32]byte{4}}, {"index", []uint32{1, 2}, [32]byte{5}}, {"owner state", nil, [32]byte{6}}},
			out: []output{{kind: writeOutput, key: "//lib", name: "srcs", text: `["a.ts"]`}, {kind: ownerOutput, key: "//app", text: "3"},
				{kind: logOutput, text: "say\n"}, {kind: factsOutput, name: "resolve", text: "x"}}},
		{kind: "first pass", key: "//app:app", inputs: [32]byte{6}},
		{kind: "completion", key: "//gen", never: "fatal"},
	}
}

// sealedMemoStore closes a store's bytes with the checksum a reader checks first.
func sealedMemoStore(b memoBuf) []byte {
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

func TestMemoStore_RoundTripsStorableEntriesAndReadsDamagedOrForeignStoresAsAbsent(t *testing.T) {
	entries := sampleEntries()
	data := encodeMemoStore("key", "binary", sampleArgs, entries)
	write := func(data []byte) string {
		path := filepath.Join(t.TempDir(), "memo")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	store := openMemoStore(write(data), "key", "binary")
	if store == nil {
		t.Fatal("a store just encoded is not read back")
	}
	for _, want := range entries[:2] {
		if got, ok := store.entry(taskID{want.kind, want.key}); !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("entry %s %s = %+v (%v), want %+v", want.kind, want.key, got, ok, want)
		}
	}
	if _, ok := store.entry(taskID{"completion", "//gen"}); ok {
		t.Error("a task with something in never was stored")
	}
	// One stored task, the kind "completion" its string table's first string, at off with n bytes.
	head := func(off, n int) memoBuf {
		return memoBuf(memoStoreMagic).str("key").str("binary").num(0).num(0).num(1).str("completion").num(1).num(0).str("//lib").num(off).num(n)
	}
	short := openMemoStore(write(sealedMemoStore(head(0, 2).num(1).num(1))), "key", "binary")
	if short == nil {
		t.Fatal("a store of one short entry is not read")
	}
	if _, ok := short.entry(taskID{"completion", "//lib"}); ok {
		t.Error("an entry cut short was decoded")
	}
	past := append(head(4, 6), "12345678"...)
	hostile := openMemoStore(write(sealedMemoStore(append(head(0, 3000).sum([32]byte{}).num(2000), make([]byte, 2999)...))), "key", "binary")
	if hostile == nil {
		t.Fatal("a store of one hostile entry is not read")
	}
	noStrings := memoBuf(memoStoreMagic).str("key").str("binary").num(1).str("owner state").num(0).num(0).num(1).num(0).str("//lib").num(0).num(0)
	if openMemoStore(write(sealedMemoStore(noStrings)), "key", "binary") != nil {
		t.Error("an index naming a kind past an empty string table was read as a store")
	}
	noOracle := openMemoStore(write(sealedMemoStore(append(memoBuf(memoStoreMagic).str("key").str("binary").num(0).num(0).num(1).str("completion").
		num(1).num(0).str("//lib").num(0).num(35), memoBuf(nil).sum([32]byte{}).num(0).num(1).num(0)...))), "key", "binary")
	if noOracle == nil {
		t.Fatal("a store of one entry naming no oracle is not read")
	}
	if _, ok := noOracle.entry(taskID{"completion", "//lib"}); ok {
		t.Error("a question naming an oracle past the oracle table was decoded")
	}
	if allocs := testing.AllocsPerRun(10, func() { hostile.entry(taskID{"completion", "//lib"}) }); allocs > 5 {
		t.Errorf("decoding an entry that claims more reads than its bytes hold allocated %v times", allocs)
	}
	flipped := bytes.Clone(data)
	flipped[len(flipped)/2] ^= 1
	for _, c := range []struct {
		name string
		path string
		key  string
		bin  string
	}{
		{"another flag set's store", write(data), "other", "binary"},
		{"another binary's store", write(data), "key", "other"},
		{"a store with a flipped byte", write(flipped), "key", "binary"},
		{"a truncated store", write(data[:len(data)-9]), "key", "binary"},
		{"an index pointing past the entries", write(sealedMemoStore(past)), "key", "binary"},
		{"an empty file", write(nil), "key", "binary"},
		{"no file", filepath.Join(t.TempDir(), "absent"), "key", "binary"},
	} {
		if openMemoStore(c.path, c.key, c.bin) != nil {
			t.Errorf("%s was read as a store", c.name)
		}
	}
}

// verifyPass runs task as a task of a -ts_memo=verify pass against the store the entries were written to,
// and returns the recorder.
func verifyPass(t *testing.T, s *programStore, file *storeFile, task func()) *taskRecorder {
	t.Helper()
	s.recorder = newTaskRecorder(true)
	s.recorder.store = file
	file.startLoad()
	s.verified("completion", "//lib", nil, task)
	return s.recorder
}

// Not parallel: a recorded task captures the process-wide log, and memoFailed is replaced.
func TestMemo_VerifyFailsOnlyWhenAStoredEntryWhoseInputsReadsAndQuestionsHoldGivesOtherOutputs(t *testing.T) {
	var failed string
	memoFailed = func(format string, a ...any) { failed = fmt.Sprintf(format, a...) }
	t.Cleanup(func() { memoFailed = log.Fatalf })
	read := func(s *programStore, say string) func() {
		return func() {
			s.semanticRule("//lib")
			s.buildFile("lib")
			s.observedOutputs("gen")
			s.storedHoldings("")
			s.protoProviderIndex()
			ask("probe", []string{"x"}, "answer")
			noteOutput(func() []output { return []output{{kind: logOutput, text: say}} })
		}
	}
	type row struct {
		name, say, outcome, failure string
		base, change                func(s *programStore)
	}
	stale := func(name string, change func(s *programStore)) row {
		return row{name: name + " and the outputs differ", say: "b", outcome: "stale reads", change: change}
	}
	wrapper := func(proto string) func(s *programStore) {
		return func(s *programStore) {
			getConfig(s.inputs["lib"].config).protos = newProtoStore()
			w := rule.NewRule("ts_proto_library", "w")
			w.SetAttr("out_dir", "gen")
			w.SetAttr("proto", proto)
			s.emission.setRule("//lib:w", w)
		}
	}
	for _, c := range []row{
		{name: "nothing changed", say: "a", outcome: "clean"},
		stale("a read rule changed", func(s *programStore) { s.emission.rules["//lib"].SetAttr("srcs", []string{"m.ts"}) }),
		stale("a read directory declares another output", func(s *programStore) { replaceGen(s, "x.ts", "y.ts") }),
		stale("a rule leaves a read BUILD file", func(s *programStore) { s.emission.files["lib"].Rules = nil }),
		stale("a key joins the store", func(s *programStore) {
			s.inputs["new"] = s.inputs["lib"]
			s.emission.setRule("//new", rule.NewRule("ts_compile", "new"))
		}),
		{name: "a proto wrapper names another native and the outputs differ", say: "b", outcome: "stale reads", base: wrapper(":p"), change: wrapper(":q")},
		{name: "everything held and the outputs differ", say: "b", outcome: "clean", failure: "missing read: completion //lib: stored log a, now log b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			failed = ""
			file := &storeFile{path: filepath.Join(t.TempDir(), "memo"), key: "k", binary: "b"}
			before := readsetStore(t)
			if c.base != nil {
				c.base(before)
			}
			recordTask(before, read(before, "a"))
			if err := os.WriteFile(file.path, encodeMemoStore(file.key, file.binary, before.recorder.questions.questionArgs, before.recorder.entries), 0o644); err != nil {
				t.Fatal(err)
			}
			s := readsetStore(t)
			if c.base != nil {
				c.base(s)
			}
			if c.change != nil {
				c.change(s)
			}
			rec := verifyPass(t, s, file, read(s, c.say))
			if rec.tally[c.outcome] != 1 || !strings.Contains(failed, c.failure) || (c.failure == "") != (failed == "") {
				t.Fatalf("tally %v, failure %q; want one %q and failure %q", rec.tally, failed, c.outcome, c.failure)
			}
			if _, err := os.Stat(file.path); (err != nil) != (c.failure != "") {
				t.Fatalf("store removed = %v, want %v: a store a run found wrong is dropped, any other is kept", err != nil, c.failure != "")
			}
		})
	}
	for _, c := range []struct {
		name, outcome string
		stored        bool
		task          func()
	}{
		{"a question answered differently", "stale questions", true, func() {
			activeRun.inputs = [32]byte{1}
			ask("probe", []string{"x"}, "other")
		}},
		{"an input that changed", "stale inputs", true, func() {
			activeRun.inputs = [32]byte{9}
			ask("probe", []string{"x"}, "answer")
		}},
		{"a task that can no longer be stored", "not storable", true, func() { activeRun.keepOut("fatal") }},
		{"a task with no stored entry", "new", false, func() {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			failed = ""
			file := &storeFile{path: filepath.Join(t.TempDir(), "memo"), key: "k", binary: "b"}
			if c.stored {
				entry := taskEntry{kind: "completion", key: "//lib", inputs: [32]byte{1}, questions: []question{askedOnce("probe", "\x00x", "answer")}}
				table := questionArgs{oracles: []string{"probe"}, oracleOf: []uint32{0}, args: []string{"\x00x"}}
				if err := os.WriteFile(file.path, encodeMemoStore("k", "b", table, []taskEntry{entry}), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rec := verifyPass(t, readsetStore(t), file, c.task)
			if rec.tally[c.outcome] != 1 || failed != "" {
				t.Fatalf("tally %v, failure %q; want one %q", rec.tally, failed, c.outcome)
			}
		})
	}
}

func TestMemo_APassThatRanNoTaskLeavesTheLastStoreAndOneThatDidReplacesIt(t *testing.T) {
	file := &storeFile{path: filepath.Join(t.TempDir(), "memo"), key: "k", binary: "b"}
	if err := os.WriteFile(file.path, []byte("last pass"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newTaskRecorder(false)
	rec.store = file
	rec.startStore()
	rec.waitStore()
	if kept, _ := os.ReadFile(file.path); string(kept) != "last pass" {
		t.Fatalf("a pass with no entry replaced the store with %q", kept)
	}
	rec.entries = sampleEntries()
	rec.startStore()
	rec.waitStore()
	if openMemoStore(file.path, "k", "b") == nil {
		t.Fatal("a pass with entries did not write a store a later pass reads")
	}
}

// poisonMemoStore gives every stored entry an output no pass produces, so a pass that opens the store and
// finds the entry clean fails.
func poisonMemoStore(t *testing.T, cache string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(cache, memoStoreName+"-*"))
	if len(files) != 1 {
		t.Fatalf("store files %v, want one", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	header := &memoReader{s: string(data[len(memoStoreMagic):])}
	key, binaryID := header.str(), header.str()
	store := openMemoStore(files[0], key, binaryID)
	if store == nil {
		t.Fatal("the store the run wrote does not open")
	}
	var entries []taskEntry
	kept := map[outputKind]bool{}
	for id := range store.spans {
		e, _ := store.entry(id)
		for _, o := range e.out {
			kept[o.kind] = true
		}
		entries = append(entries, taskEntry{kind: e.kind, key: e.key, inputs: e.inputs, reads: e.reads, questions: e.questions,
			out: append(e.out, output{kind: logOutput, text: "poison"})})
	}
	have := map[string]bool{}
	for id := range store.spans {
		have[id.kind] = true
	}
	if !have["first pass"] || !have["completion"] {
		t.Errorf("the stored tasks are %v, want first-pass edge resolutions and completions", have)
	}
	for _, kind := range []outputKind{writeOutput, factsOutput} {
		if !kept[kind] {
			t.Errorf("no stored task has a %s output", kind)
		}
	}
	if err := os.WriteFile(files[0], encodeMemoStore(key, binaryID, store.questionArgs, entries), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Not parallel: it runs the Gazelle binary.
func TestMemo_OnlyAFullPassOpensTheStoreAndVerifiesWhatItFinds(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	run := func(args ...string) (string, error) {
		return tsGazelle(t, root, append([]string{"-ts_listing_cache=" + cache, "-ts_memo=verify"}, args...)...)
	}
	if out, err := run("-ts_memo=record"); err != nil {
		t.Fatalf("first pass: %v\n%s", err, out)
	}
	time.Sleep(pastRacyWindow)
	if out, err := run(); err != nil || !strings.Contains(out, "-ts_memo=verify: ") || regexp.MustCompile(`[1-9][0-9]* new \(`).MatchString(out) {
		t.Fatalf("a verify pass found no store a record pass wrote (%v):\n%s", err, out)
	}
	fullPass := func() (string, error) {
		runRecords, _ := filepath.Glob(filepath.Join(cache, runRecordName+"-*"))
		for _, file := range runRecords {
			os.Remove(file)
		}
		return run("-ts_resolve_record=off")
	}
	if out, err := fullPass(); err != nil || !regexp.MustCompile(`-ts_memo=verify: .*[1-9][0-9]* clean \(`).MatchString(out) {
		t.Fatalf("a full pass from the converged tree found no clean task in the last pass's store (%v):\n%s", err, out)
	}
	if out, err := run("-ts_resolve_record=off"); err != nil || strings.Contains(out, "-ts_memo=") {
		t.Fatalf("a replay of that pass failed (%v) or printed its tally:\n%s", err, out)
	}
	time.Sleep(pastRacyWindow)
	if out, err := run(); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	poisonMemoStore(t, cache)
	stored := func() []byte {
		files, _ := filepath.Glob(filepath.Join(cache, memoStoreName+"-*"))
		data, _ := os.ReadFile(files[0])
		return data
	}
	poisoned := stored()
	for _, edit := range []struct{ name, file, body string }{
		{"no change", "", ""},
		{"a body edit", "app/c.ts", "import { b } from './b';\nexport const c = b + 1;\n"},
		{"one import", "app/d.ts", "import { b } from './b';\nexport const d = b;\n"},
		{"a rewritten BUILD file", "app/BUILD.bazel", "?"},
		{"a new file", "app/g.ts", "import { b } from './b';\nexport const g = b;\n"},
	} {
		if edit.body == "?" {
			data, err := os.ReadFile(filepath.Join(root, edit.file))
			if err != nil {
				t.Fatal(err)
			}
			edit.body = string(data)
		}
		if edit.file != "" {
			writeFile(t, filepath.Join(root, edit.file), edit.body)
		}
		if out, err := run(); err != nil {
			t.Fatalf("%s: %v\n%s", edit.name, err, out)
		}
		if !bytes.Equal(stored(), poisoned) {
			t.Fatalf("%s ran a full pass, which opens and rewrites the store", edit.name)
		}
	}
	for file, body := range map[string]string{"app/c.ts": fastPathTree()["app/c.ts"], "app/d.ts": fastPathTree()["app/d.ts"]} {
		writeFile(t, filepath.Join(root, file), body)
	}
	os.Remove(filepath.Join(root, "app/g.ts"))
	out, err := fullPass()
	if err == nil || !strings.Contains(out, "missing read: ") {
		t.Fatalf("a full pass did not fail on a poisoned entry (%v):\n%s", err, out)
	}
	if files, _ := filepath.Glob(filepath.Join(cache, memoStoreName+"-*")); len(files) != 0 {
		t.Fatalf("the store a run found wrong was kept: %v", files)
	}
}

// Not parallel: it runs the Gazelle binary.
func TestMemo_ARunOverOneDirectoryTalliesButKeepsNoStore(t *testing.T) {
	requireTsgo(t)
	root := writeTree(t, fastPathTree())
	cache := t.TempDir()
	for _, run := range []struct {
		args   []string
		stores int
	}{{[]string{"app", "."}, 0}, {nil, 1}} {
		out, err := tsGazelle(t, root, append([]string{"-ts_listing_cache=" + cache, "-ts_memo=record"}, run.args...)...)
		if err != nil || !strings.Contains(out, "-ts_memo=record: ") {
			t.Fatalf("gazelle %q: %v\n%s", run.args, err, out)
		}
		if stores, _ := filepath.Glob(filepath.Join(cache, memoStoreName+"-*")); len(stores) != run.stores {
			t.Fatalf("after gazelle %q the cache holds stores %v, want %d", run.args, stores, run.stores)
		}
	}
}

// Not parallel: it runs the Gazelle binary.
func TestMemo_TheOtherVerifyModesStillAgreeWithAFullPassThatTalliesItsTasks(t *testing.T) {
	requireTsgo(t)
	for _, mode := range []string{"record", "verify"} {
		t.Run(mode, func(t *testing.T) {
			root := writeTree(t, runRecordTree())
			cache := t.TempDir()
			run := func(args ...string) (string, error) {
				return tsGazelle(t, root, append([]string{"-ts_listing_cache=" + cache, "-ts_memo=" + mode}, args...)...)
			}
			if out, err := run(); err != nil {
				t.Fatalf("first pass: %v\n%s", err, out)
			}
			time.Sleep(pastRacyWindow)
			if out, err := run(); err != nil {
				t.Fatalf("second pass: %v\n%s", err, out)
			}
			if out, err := run("-ts_run_record=verify"); err != nil || !strings.Contains(out, "run record: verified") {
				t.Fatalf("-ts_run_record=verify failed beside -ts_memo=%s (%v):\n%s", mode, err, out)
			}
			if out, err := run("-ts_readsets=verify"); err != nil || !strings.Contains(out, "-ts_memo="+mode+": ") {
				t.Fatalf("-ts_readsets=verify failed or tallied no task beside -ts_memo=%s (%v):\n%s", mode, err, out)
			}
		})
	}
}

// Not parallel: it runs the Gazelle binary, which a verify run also does.
func TestMemo_RecordRunTalliesTasksAndLeavesTheBuildFilesAsOffWrites(t *testing.T) {
	requireTsgo(t)
	root := t.TempDir()
	writeWorkspace(t, root, runRecordTree())
	other := copyTree(t, root)
	if output, err := tsGazelle(t, other); err != nil {
		t.Fatalf("gazelle: %v\n%s", err, output)
	}
	output, err := tsGazelle(t, root, "-ts_memo=record")
	if err != nil {
		t.Fatalf("gazelle -ts_memo=record: %v\n%s", err, output)
	}
	if !regexp.MustCompile(`-ts_memo=record: [1-9][0-9]* tasks \([0-9]+ ms\), [1-9][0-9]* storable, never stored: [^;]*; [1-9][0-9]* reads, [1-9][0-9]* questions`).MatchString(output) {
		t.Fatalf("record run tallied no stored task with reads and questions:\n%s", output)
	}
	builds := func(dir string) map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && entry.Name() == "BUILD.bazel" {
				text, readErr := os.ReadFile(path)
				rel, _ := filepath.Rel(dir, path)
				out[rel], err = string(text), readErr
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if diff := snapshotDiff(builds(other), builds(root)); diff != "" {
		t.Fatalf("-ts_memo=record changed the BUILD files:\n%s", diff)
	}
}
