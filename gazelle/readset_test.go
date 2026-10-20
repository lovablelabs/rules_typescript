package typescript

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

func readsetStore(t *testing.T) *programStore {
	t.Helper()
	s := newProgramStore()
	s.repoConfig = config.New()
	for pkg, src := range map[string]string{
		"app": "ts_compile(name = \"app\", srcs = [\"a.ts\"])\n",
		"lib": "ts_compile(name = \"lib\", srcs = [\"l.ts\"])\n",
		"gen": "genrule(name = \"gen\", outs = [\"x.ts\"])\n",
	} {
		f, err := rule.LoadData(pkg+"/BUILD.bazel", pkg, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		s.generatedPackages[pkg] = true
		s.recordBuild(s.repoConfig, pkg, f)
		c := config.New()
		c.Exts[languageName] = &tsConfig{programs: s}
		s.inputs[pkg] = programInput{config: c}
	}
	return s
}

func replaceGen(s *programStore, outs ...string) {
	r := rule.NewRule("genrule", "gen")
	r.SetAttr("outs", outs)
	f := s.emission.files["gen"]
	f.Rules[0] = r
	s.emission.setRule("//gen", r)
}

// Each row is one write between a speculation and its commit. Not parallel: speculation captures the
// process-wide log.
func TestReadSet_StaleNamesTheWriteThatMovedARecordedRead(t *testing.T) {
	for _, c := range []struct {
		name, want, unstamped string
		write                 func(s *programStore)
	}{
		{name: "nothing written", write: func(*programStore) {}},
		{name: "an unread rule replaced", write: func(s *programStore) {
			s.emission.setRule("//app", rule.NewRule("ts_compile", "app"))
		}},
		{name: "a read rule replaced", want: "rule", write: func(s *programStore) {
			s.emission.setRule("//lib", rule.NewRule("ts_compile", "lib"))
		}},
		{name: "a read rule deleted", want: "rule", write: func(s *programStore) { s.emission.deleteRule("//lib") }},
		{name: "a read rule promoted to emit by a completion", want: "rule", write: func(s *programStore) {
			s.completePrograms["//lib"] = func(*programCompletion) {}
			(&programCompletion{s: s, rooted: true}).ownerMode("//lib")
		}},
		{name: "a read rule edited in place without a stamp", unstamped: "//lib", write: func(s *programStore) {
			s.emission.rules["//lib"].SetAttr("emit", true)
		}},
		{name: "a read directory stored again with the same outputs", write: func(s *programStore) { replaceGen(s, "x.ts") }},
		{name: "a read directory declared another output", want: "outputs", write: func(s *programStore) { replaceGen(s, "x.ts", "y.ts") }},
		{name: "the selected-imports memo filled in on a read rule", write: func(s *programStore) {
			s.emission.rules["//lib"].SetPrivateAttr("_ts_selected_imports", map[resolve.ImportSpec]bool{})
		}},
		{name: "a rule taken out of a read BUILD file", want: "file", write: func(s *programStore) {
			s.emission.files["lib"].Rules = nil
		}},
		{name: "a key joined the store", want: "keys", write: func(s *programStore) {
			s.emission.setRule("//new", rule.NewRule("ts_compile", "new"))
		}},
		{name: "an unread rule changed kind", want: "keys", write: func(s *programStore) {
			s.emission.setRule("//app", rule.NewRule("ts_test", "app"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := readsetStore(t)
			run := s.speculateTask(func() {
				s.semanticRule("//lib")
				s.buildFile("lib")
				s.observedOutputs("gen")
				s.storedHoldings("")
			})
			if run.bail != "" {
				t.Fatalf("speculation bailed: %s", run.bail)
			}
			c.write(s)
			got, err := run.reads.stale(s)
			if c.unstamped != "" {
				if err == nil || !strings.Contains(err.Error(), c.unstamped) {
					t.Fatalf("stale = %q, %v; want the unstamped write to %s refused", got, err, c.unstamped)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("stale = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// Not parallel: speculation captures the process-wide log and deferred fatals.
func TestReadSet_SpeculationWritesNothingAndBailsOnWhatOnlyTheSerialRunMayDo(t *testing.T) {
	s := readsetStore(t)
	before, stamp := ruleText(s.emission.rules["//lib"]), s.emission.stamps["//lib"]
	pending, _ := deferredFatalMessage()
	run := s.speculateTask(func() {
		lib := s.emission.rules["//lib"]
		activeRun.own(lib, lib).SetAttr("deps", []string{"//app"})
		selectedRuleImports(s.inputs["app"].config, nil, s.emission.rules["//app"], "app")
		deferFatal(label.New("", "lib", "lib"), "lib/l.ts", "app/a.ts", "held back")
		noteOutput(func() []string { return []string{ruleText(s.semanticRule("//lib"))} })
	})
	if after := ruleText(s.emission.rules["//lib"]); after != before || s.emission.stamps["//lib"] != stamp {
		t.Fatalf("speculation wrote the store: %s (stamp %d), want %s (stamp %d)", after, s.emission.stamps["//lib"], before, stamp)
	}
	if memo := s.emission.rules["//app"].PrivateAttr("_ts_selected_imports"); memo != nil {
		t.Fatalf("speculation stored the selected imports memo %v on //app", memo)
	}
	if message, _ := deferredFatalMessage(); message != pending {
		t.Fatalf("speculation deferred %q for the run", message)
	}
	if read := run.out[len(run.out)-1]; !strings.Contains(read, `deps=["//app"]`) {
		t.Fatalf("speculation read %q through the store, want its own clone's deps", read)
	}
	for _, c := range []struct {
		name, want string
		task       func()
	}{
		{"a fatal", "fatal", func() { fatalf("unreachable") }},
		{"an install check the run has not made", "install check", func() { s.requireInstall(t.TempDir()) }},
		{"a pending owner", "pending owner", func() {
			s.completePrograms["//lib"] = func(*programCompletion) {}
			(&programCompletion{s: s}).ownerMode("//lib")
		}},
	} {
		if got := s.speculateTask(c.task).bail; got != c.want {
			t.Errorf("%s: bail %q, want %q", c.name, got, c.want)
		}
	}
	if s.installChecked {
		t.Error("speculation marked the install checked, so the serial run would skip it")
	}
}

// Not parallel: it replaces readsetFailed and captures the process-wide log.
func TestReadSet_VerifyFailsOnlyWhenHeldReadsGaveDifferentOutputs(t *testing.T) {
	var failed string
	readsetFailed = func(format string, a ...any) { failed = fmt.Sprintf(format, a...) }
	t.Cleanup(func() { readsetFailed = log.Fatalf })
	output := func(out string) func() { return func() { noteOutput(func() []string { return []string{out} }) } }
	for _, c := range []struct {
		name, serial, tally, failure string
		write                        func(s *programStore)
		bail                         bool
	}{
		{name: "reads held and the outputs agree", serial: "a", tally: "completion agreed"},
		{name: "reads held and the outputs differ", serial: "b", tally: "completion agreed", failure: "speculated: a\nserial:     b"},
		{name: "a read moved and the outputs differ", serial: "b", tally: "completion conflict rule", write: func(s *programStore) {
			s.emission.setRule("//lib", rule.NewRule("ts_compile", "lib"))
		}},
		{name: "the speculation bailed", serial: "b", tally: "completion bail fatal", bail: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			failed = ""
			s := readsetStore(t)
			s.verify = &readsetVerifier{specs: map[any]*taskRun{}, tally: map[string]int{}, took: map[string]time.Duration{}}
			spec := s.speculateTask(func() {
				s.semanticRule("//lib")
				output("a")()
				if c.bail {
					fatalf("unreachable")
				}
			})
			if c.write != nil {
				c.write(s)
			}
			s.verified("completion", "//lib", spec, output(c.serial))
			if s.verify.tally[c.tally] != 1 || !strings.Contains(failed, c.failure) || (c.failure == "") != (failed == "") {
				t.Fatalf("tally %v, failure %q; want one %q and failure %q", s.verify.tally, failed, c.tally, c.failure)
			}
		})
	}
	t.Run("a completion that asks for its own emission mode is speculated", func(t *testing.T) {
		s := readsetStore(t)
		s.verify = &readsetVerifier{specs: map[any]*taskRun{}, tally: map[string]int{}, took: map[string]time.Duration{}}
		s.completePrograms["//lib"] = func(cx *programCompletion) { cx.ownerMode("//lib") }
		s.speculatePending("", false, nil, true)
		if spec := s.verify.specs["//lib"]; spec.bail != "" || s.completePrograms["//lib"] == nil {
			t.Fatalf("//lib speculated with bail %q waiting on %q, pending again: %v", spec.bail, spec.waiting, s.completePrograms["//lib"] != nil)
		}
	})
	t.Run("a speculation that waited on an owner is speculated again once the owner completed", func(t *testing.T) {
		s := readsetStore(t)
		s.verify = &readsetVerifier{specs: map[any]*taskRun{}, tally: map[string]int{}, took: map[string]time.Duration{}}
		s.completePrograms["//app"] = func(cx *programCompletion) { cx.ownerMode("//lib") }
		s.completePrograms["//lib"] = func(*programCompletion) {}
		s.speculatePending("", false, nil, true)
		if spec := s.verify.specs["//app"]; spec.waiting != "//lib" {
			t.Fatalf("//app speculated with bail %q waiting on %q, want it waiting on //lib", spec.bail, spec.waiting)
		}
		delete(s.completePrograms, "//lib")
		s.speculatePending("", false, nil, false)
		if spec := s.verify.specs["//app"]; spec.bail != "" {
			t.Fatalf("//app still bails with %q after //lib completed", spec.bail)
		}
	})
}

// A verify run that speculates nothing, or never compares, would pass every BUILD file through unchecked.
// Gazelle before 0.54 resolves rule by rule, so no first-pass task is speculated or tallied.
func TestReadSet_VerifyRunComparesFirstPassAndCompletionTasks(t *testing.T) {
	requireTsgo(t)
	t.Parallel()
	root := t.TempDir()
	writeWorkspace(t, root, runRecordTree())
	output, err := tsGazelle(t, root, "-ts_readsets=verify")
	if err != nil {
		t.Fatalf("gazelle -ts_readsets=verify: %v\n%s", err, output)
	}
	tally := regexp.MustCompile(`-ts_readsets=verify: (.*)`).FindStringSubmatch(output)
	agreed := func(kind string) bool {
		return regexp.MustCompile(`[1-9][0-9]* ` + kind + ` agreed`).MatchString(tally[1])
	}
	if tally == nil || strings.Contains(tally[1], "first pass") && !agreed("first pass") || !agreed("completion") {
		t.Fatalf("verify compared no first-pass or no completion task:\n%s", output)
	}
}
