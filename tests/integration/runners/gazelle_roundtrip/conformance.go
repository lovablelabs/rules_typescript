package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mikn/rules_typescript/tests/integration/harness"
)

func conformanceDeadline(it *harness.IT, started time.Time) time.Time {
	seconds, err := strconv.ParseInt(os.Getenv("TEST_TIMEOUT"), 10, 64)
	if err != nil || seconds <= 0 || seconds > int64((45*time.Minute)/time.Second) {
		it.Fail("conformance requires an enclosing Bazel TEST_TIMEOUT of at most 45 minutes")
	}
	return started.Add(time.Duration(seconds) * time.Second)
}

func formalConformance(it *harness.IT, deadline time.Time) {
	var tools struct {
		Java, TLC, Canonical, Producer string
		Models                         []string
		Workers                        int
	}
	readConformanceJSON(it, it.Runfile(os.Getenv("TEST_WORKSPACE")+"/tests/integration/formal_tools.json"), &tools)
	output := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if !filepath.IsAbs(output) {
		it.Fail("conformance requires Bazel's undeclared outputs directory")
	}
	output = filepath.Join(output, "conformance")
	models := filepath.Join(output, "models")
	for _, model := range tools.Models {
		it.Write(filepath.Join(models, filepath.Base(model)), it.Read(it.Runfile(model)))
	}
	for _, phase := range []struct{ name, executable string }{{"canonical", tools.Canonical}, {"producer", tools.Producer}} {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			it.Fail("enclosing test deadline expired before %s conformance", phase.name)
		}
		args := []string{
			"-java", it.Runfile(tools.Java), "-tlc", it.Runfile(tools.TLC), "-models", models,
			"-output", filepath.Join(output, phase.name), "-deadline", deadline.Format(time.RFC3339Nano),
			"-workers", strconv.Itoa(tools.Workers), "-cleanup", remaining.String(),
		}
		if phase.name == "producer" {
			args = append(args, "-node-modules", "//:node_modules", "-node-types-dep", "@npm//:types_node", "-vitest-dep", "@npm//:vitest")
		}
		log, err := it.Exec("formal_"+phase.name, nil, it.Runfile(phase.executable), args...)
		it.Write(filepath.Join(output, phase.name+".log"), log.Text)
		if err != nil {
			log.Dump()
			it.Fail("%s native model conformance: %v", phase.name, err)
		}
	}
	var exported struct {
		Status         string   `json:"status"`
		Workspace      string   `json:"workspace_fragment"`
		Targets        []string `json:"analysis_test_targets"`
		ExpectedCount  int      `json:"expected_test_count"`
		RuntimeTargets []struct {
			Label  string `json:"label"`
			Runner string `json:"runner"`
		} `json:"native_runtime_targets"`
		RuntimeBodies []string `json:"required_test_bodies_per_target"`
	}
	readConformanceJSON(it, filepath.Join(output, "producer", "export.json"), &exported)
	if exported.Status != "exported" || len(exported.Targets) == 0 || len(exported.Targets) != exported.ExpectedCount || len(exported.RuntimeTargets) == 0 || len(exported.RuntimeBodies) == 0 {
		it.Fail("incomplete producer export manifest")
	}
	fixture := it.Path("producer_cases")
	if _, err := os.Lstat(fixture); !os.IsNotExist(err) {
		it.Fail("producer fixture destination already exists: %s", fixture)
	}
	if err := os.CopyFS(it.WorkspaceDir, os.DirFS(exported.Workspace)); err != nil {
		it.Fail("stage producer trace fixture: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(fixture); err != nil {
			it.Fail("remove producer trace fixture: %v", err)
		}
	}()
	conformanceBazel(it, output, "analysis", exported.Targets, nil)
	for _, layout := range []string{"enable_runfiles", "noenable_runfiles"} {
		for index, target := range exported.RuntimeTargets {
			var reporter string
			switch target.Runner {
			case "node_test":
				reporter = "--test-reporter=junit"
			case "vitest":
				reporter = "--reporter=junit"
			default:
				it.Fail("unknown native runner %q", target.Runner)
			}
			name := fmt.Sprintf("runtime_%s_%d", layout, index)
			log := conformanceBazel(it, output, name, []string{target.Label}, []string{"--" + layout, "--test_arg=" + reporter})
			requireConformanceBodies(it, target.Label, log.Text, exported.RuntimeBodies)
		}
	}
	it.Pass("native canonical admission and producer trace analysis/runtime replay")
}

func readConformanceJSON(it *harness.IT, path string, target any) {
	decoder := json.NewDecoder(strings.NewReader(it.Read(path)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		it.Fail("decode %s: %v", path, err)
	}
}

func conformanceBazel(it *harness.IT, output, name string, targets, flags []string) *harness.Log {
	events := it.Scratch("formal_" + name + ".bep.json")
	args := []string{"test", "--nocache_test_results", "--flaky_test_attempts=1", "--runs_per_test=1", "--test_output=all", "--build_event_json_file=" + events}
	args = append(args, flags...)
	args = append(args, targets...)
	log, err := it.BazelLog("formal_"+name, args...)
	it.Write(filepath.Join(output, name+".log"), log.Text)
	if err != nil {
		log.Dump()
		it.Fail("producer replay %s: %v", name, err)
	}
	passed := map[string]bool{}
	invocation := ""
	decoder := json.NewDecoder(strings.NewReader(it.Read(events)))
	for {
		var event struct {
			ID struct {
				Summary struct{ Label string } `json:"testSummary"`
			}
			Summary struct{ OverallStatus string } `json:"testSummary"`
			Started struct{ UUID string }
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			it.Fail("decode native Bazel events: %v", err)
		}
		if event.Started.UUID != "" {
			invocation = event.Started.UUID
		}
		if label := event.ID.Summary.Label; label != "" {
			if event.Summary.OverallStatus != "PASSED" || passed[label] {
				it.Fail("%s: unexpected native test summary for %s: %s", name, label, event.Summary.OverallStatus)
			}
			passed[label] = true
		}
	}
	if len(passed) != len(targets) {
		it.Fail("%s: %d native test results, expected %d", name, len(passed), len(targets))
	}
	for _, target := range targets {
		if !passed[target] {
			it.Fail("%s: missing native test result for %s", name, target)
		}
	}
	results, err := json.MarshalIndent(struct {
		Invocation string          `json:"invocation_id"`
		Passed     map[string]bool `json:"passed"`
	}{invocation, passed}, "", "  ")
	if err != nil {
		it.Fail("encode native test results: %v", err)
	}
	it.Write(filepath.Join(output, name+".results.json"), string(results)+"\n")
	return log
}

func requireConformanceBodies(it *harness.IT, target, output string, bodies []string) {
	start := strings.Index(output, "<testsuite")
	if start < 0 {
		it.Fail("%s: native runner emitted no JUnit test cases", target)
	}
	var suite conformanceSuite
	if err := xml.NewDecoder(strings.NewReader(output[start:])).Decode(&suite); err != nil {
		it.Fail("%s: decode native runner JUnit: %v", target, err)
	}
	counts := map[string]int{}
	var visit func(conformanceSuite)
	visit = func(s conformanceSuite) {
		for _, c := range s.Cases {
			if c.Failure != nil || c.Error != nil || c.Skipped != nil {
				it.Fail("%s: native test body %q did not pass", target, c.Name)
			}
			counts[c.Name]++
		}
		for _, child := range s.Suites {
			visit(child)
		}
	}
	visit(suite)
	for _, body := range bodies {
		if counts[body] != 1 {
			it.Fail("%s: native body %q passed %d times, expected once", target, body, counts[body])
		}
	}
}

type conformanceSuite struct {
	Cases  []conformanceTestCase `xml:"testcase"`
	Suites []conformanceSuite    `xml:"testsuite"`
}

type conformanceTestCase struct {
	Name    string    `xml:"name,attr"`
	Failure *struct{} `xml:"failure"`
	Error   *struct{} `xml:"error"`
	Skipped *struct{} `xml:"skipped"`
}
