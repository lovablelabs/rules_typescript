package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/mikn/rules_typescript/tools/tlatrace"
)

type input struct {
	Name            string     `json:"name"`
	Kind            string     `json:"kind"`
	RuntimeIdentity string     `json:"runtime_id"`
	ProducerPackage []string   `json:"producer_package"`
	ConsumerPackage []string   `json:"consumer_package"`
	SourcePath      []string   `json:"source_path"`
	RuntimePath     []string   `json:"runtime_path"`
	Request         string     `json:"request"`
	RequestPath     []string   `json:"request_path"`
	Edge            string     `json:"edge"`
	ForeignSources  [][]string `json:"foreign_sources"`
	Aliases         [][]string `json:"aliases"`
	Origins         [][]string `json:"origins"`
	ProducerFiles   []string   `json:"producer_files"`
	ProducerJS      []string   `json:"producer_js"`
	Scope           bool       `json:"scope"`
	Shadow          bool       `json:"shadow"`
	Member          bool       `json:"member"`
	Repeat          bool       `json:"repeat"`
	Peer            bool       `json:"peer"`
	Injected        bool       `json:"injected"`
	Runner          string     `json:"runner"`
	LocalPath       []string   `json:"local_path"`
	Roots           []string   `json:"roots"`
	ExtraInputs     []string   `json:"extra_inputs"`
	LocalOrigins    [][]string `json:"local_origins"`
}

type view struct {
	Path       []string `json:"path"`
	Kind       string   `json:"kind"`
	Coordinate []string `json:"coordinate"`
}

type observation struct {
	Error         string     `json:"error"`
	Live          []string   `json:"live"`
	Owners        []string   `json:"owners"`
	Origins       [][]string `json:"origins"`
	Views         []view     `json:"views"`
	DirectData    [][]string `json:"direct_data"`
	DirectJS      [][]string `json:"direct_js"`
	Copyable      [][]string `json:"copyable"`
	Selected      [][]string `json:"selected"`
	Discovery     [][]string `json:"discovery"`
	RunnerOrigins [][]string `json:"runner_origins"`
}

type runnerBindings struct {
	NodeModules string `json:"node_modules"`
	NodeTypes   string `json:"node_types"`
	Vitest      string `json:"vitest"`
}

type runtimeTarget struct {
	Label  string `json:"label"`
	Runner string `json:"runner"`
}

const (
	forwardedBody = "forwarded producer root executes"
	localBody     = "ordinary local root executes"
)

type state struct {
	WitnessCount int         `json:"witness_count"`
	Phase        string      `json:"phase"`
	Input        input       `json:"input"`
	Expected     observation `json:"expected"`
}

type artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type receipt struct {
	Status string        `json:"status"`
	Case   tlatrace.Case `json:"case"`
	Inputs []artifact    `json:"inputs"`
	Trace  *artifact     `json:"trace,omitempty"`
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one complete JSON value")
	}
	return nil
}

func nativeState(raw tlatrace.State) (state, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return state{}, err
	}
	var result state
	if err := decodeStrict(data, &result); err != nil {
		return state{}, err
	}
	sortRows(result.Input.Aliases)
	sortRows(result.Input.Origins)
	sortRows(result.Input.ForeignSources)
	sortRows(result.Input.LocalOrigins)
	slices.Sort(result.Input.Roots)
	slices.Sort(result.Input.ExtraInputs)
	slices.Sort(result.Input.ProducerFiles)
	slices.Sort(result.Input.ProducerJS)
	slices.Sort(result.Expected.Live)
	slices.Sort(result.Expected.Owners)
	sortRows(result.Expected.Origins)
	sortRows(result.Expected.DirectData)
	sortRows(result.Expected.DirectJS)
	sortRows(result.Expected.Copyable)
	sortRows(result.Expected.Selected)
	sortRows(result.Expected.Discovery)
	sortRows(result.Expected.RunnerOrigins)
	slices.SortFunc(result.Expected.Views, func(a, b view) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return bytes.Compare(left, right)
	})
	return result, nil
}

func sortRows(rows [][]string) {
	slices.SortFunc(rows, func(a, b []string) int { return slices.Compare(a, b) })
}

func component(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func pathWithin(path, prefix []string) bool {
	if len(path) <= len(prefix) || !slices.Equal(path[:len(prefix)], prefix) {
		return false
	}
	for _, part := range path {
		if !component(part) {
			return false
		}
	}
	return true
}

func validateState(s state) error {
	f := s.Input
	if !component(f.Name) || !slices.Contains([]string{"module", "source", "json", "asset", "asset_js", "opaque", "generated_js"}, f.Kind) || !slices.Contains([]string{"data", "srcs"}, f.Edge) || !slices.Contains([]string{"", "node_test", "vitest"}, f.Runner) {
		return errors.New("producer trace has an unsupported fixture identity or kind")
	}
	prefix := []string{"producer_cases", f.Name}
	if f.Runner != "" && !pathWithin(f.LocalPath, f.ConsumerPackage) {
		return errors.New("runner fixture omits its local test input")
	}
	paths := [][]string{f.ProducerPackage, f.ConsumerPackage, f.SourcePath, f.RuntimePath, f.RequestPath}
	if len(f.LocalPath) > 0 {
		paths = append(paths, f.LocalPath)
		if !pathWithin(f.LocalPath, f.ConsumerPackage) {
			return errors.New("local test source must belong to the consumer package")
		}
	}
	paths = append(paths, f.ForeignSources...)
	paths = append(paths, s.Expected.DirectData...)
	paths = append(paths, s.Expected.DirectJS...)
	paths = append(paths, s.Expected.Copyable...)
	for _, v := range s.Expected.Views {
		if !slices.Contains([]string{"canonical", "alias", "asset"}, v.Kind) {
			return errors.New("producer trace has an unsupported observation kind")
		}
		paths = append(paths, v.Path)
		if len(v.Coordinate) > 0 {
			paths = append(paths, v.Coordinate)
		}
	}
	for _, path := range paths {
		if !pathWithin(path, prefix) {
			return fmt.Errorf("producer trace path escapes its fixture namespace: %q", path)
		}
	}
	if !pathWithin(f.SourcePath, f.ProducerPackage) || !pathWithin(f.RuntimePath, f.ProducerPackage) || !pathWithin(f.RequestPath, f.ProducerPackage) {
		return errors.New("producer trace does not bind its source and output to the producer package")
	}
	if !slices.Contains([]string{"source", "runtime"}, f.RuntimeIdentity) {
		return errors.New("producer trace has an unbound runtime identity")
	}
	if !slices.Contains([]string{"source", "runtime", "alias"}, f.Request) {
		return errors.New("producer trace has an unbound request File")
	}
	for _, id := range append(slices.Clone(f.ProducerFiles), f.ProducerJS...) {
		if !slices.Contains([]string{"source", "runtime", "declaration"}, id) {
			return errors.New("producer trace has an unbound prior-provider File")
		}
	}
	rootIDs := []string{"source", "runtime", "local_source", "local_runtime", "alias", "other_alias"}
	for _, id := range append(slices.Clone(f.Roots), f.ExtraInputs...) {
		if !slices.Contains(rootIDs, id) {
			return errors.New("producer trace has an unbound requested root")
		}
	}
	rootPairs := append(slices.Clone(s.Expected.Selected), s.Expected.Discovery...)
	for _, pair := range append(rootPairs, s.Expected.RunnerOrigins...) {
		if len(pair) != 2 || !slices.Contains(rootIDs, pair[0]) || !slices.Contains(rootIDs, pair[1]) {
			return errors.New("producer trace has an unbound selection or discovery pair")
		}
	}
	for _, origin := range f.LocalOrigins {
		if len(origin) != 3 || !slices.Contains(rootIDs, origin[0]) || !slices.Contains(rootIDs, origin[1]) || origin[2] != "test" {
			return errors.New("producer trace has an unbound local test origin")
		}
	}
	for _, pair := range f.Aliases {
		if len(pair) != 2 {
			return errors.New("producer trace alias must have two File identities")
		}
		for _, id := range pair {
			if !slices.Contains([]string{"runtime", "alias", "other_alias", "intermediate", "other_runtime"}, id) {
				return fmt.Errorf("producer trace alias has unbound File %q", id)
			}
		}
	}
	for _, origin := range append(slices.Clone(f.Origins), s.Expected.Origins...) {
		if len(origin) != 3 || !slices.Contains([]string{"source", "other_source", "runtime"}, origin[0]) || !slices.Contains([]string{"source", "runtime"}, origin[1]) || !slices.Contains([]string{"producer", "peer", "consumer"}, origin[2]) {
			return errors.New("producer trace has an unbound source/runtime/owner tuple")
		}
	}
	for _, id := range s.Expected.Live {
		if !slices.Contains([]string{"source", "runtime"}, id) {
			return errors.New("producer trace has an unbound live File")
		}
	}
	for _, id := range s.Expected.Owners {
		if !slices.Contains([]string{"producer", "peer", "consumer"}, id) {
			return errors.New("producer trace has an unbound owner")
		}
	}
	if (s.Phase == "published") != (s.Expected.Error == "") {
		return errors.New("producer trace disposition disagrees with its exported observation")
	}
	return nil
}

func publication(trace []tlatrace.State) (state, error) {
	if len(trace) != 2 {
		return state{}, errors.New("producer trace must contain initialization and one publication")
	}
	initial, err := nativeState(trace[0])
	if err != nil {
		return state{}, err
	}
	final, err := nativeState(trace[1])
	if err != nil {
		return state{}, err
	}
	if initial.Phase != "assembling" || initial.WitnessCount != final.WitnessCount || final.WitnessCount < 1 || !slices.Contains([]string{"published", "rejected"}, final.Phase) || !reflect.DeepEqual(initial.Input, final.Input) {
		return state{}, errors.New("producer trace changes its input facts or has no publication transition")
	}
	if err := validateState(final); err != nil {
		return state{}, err
	}
	return final, nil
}

func hashFile(path string) (artifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return artifact{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return artifact{}, err
	}
	return artifact{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func export(ctx context.Context, runner tlatrace.Runner, bindings runnerBindings) error {
	manifest := filepath.Join(runner.Models, "producer_suite.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	var cases []tlatrace.Case
	if err := decodeStrict(data, &cases); err != nil {
		return err
	}
	if len(cases) == 0 {
		return errors.New("producer suite is empty")
	}
	if err := os.Mkdir(runner.Output, 0o755); err != nil {
		return err
	}
	receipts, err := os.OpenFile(filepath.Join(runner.Output, "receipts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer receipts.Close()
	var common []artifact
	for _, name := range []string{"producer_suite.json", "ProducerPublication.tla", "ProducerPublicationTrace.tla"} {
		identity, err := hashFile(filepath.Join(runner.Models, name))
		if err != nil {
			return err
		}
		common = append(common, identity)
	}
	seen := map[string]bool{}
	publications := map[string]state{}
	for _, c := range cases {
		if filepath.Base(c.Config) != c.Config || !strings.HasPrefix(c.Config, "producer_") || !strings.HasSuffix(c.Config, ".cfg") || seen[c.Config] || !slices.Contains([]string{"ProducerPublication", "ProducerPublicationTrace"}, c.Module) || c.Replay != (c.Module == "ProducerPublicationTrace") {
			return errors.New("producer suite repeats a case or has an unsupported module/replay binding")
		}
		seen[c.Config] = true
		config, err := hashFile(filepath.Join(runner.Models, c.Config))
		if err != nil {
			return err
		}
		trace, err := runner.Run(ctx, c)
		if err != nil {
			return err
		}
		record := receipt{Status: "model_checked", Case: c, Inputs: append(slices.Clone(common), config)}
		if len(trace) != 0 {
			identity, err := hashFile(filepath.Join(runner.Output, strings.TrimSuffix(c.Config, ".cfg"), "trace.json"))
			if err != nil {
				return err
			}
			record.Trace = &identity
		}
		if c.Replay {
			final, err := publication(trace)
			if err != nil {
				return fmt.Errorf("%s: %w", c.Config, err)
			}
			if _, exists := publications[final.Input.Name]; exists {
				return errors.New("native witnesses repeat a fixture identity")
			}
			publications[final.Input.Name] = final
		}
		if err := json.NewEncoder(receipts).Encode(record); err != nil {
			return err
		}
	}
	if len(publications) == 0 {
		return errors.New("producer suite exported no publication witnesses")
	}
	for _, witness := range publications {
		if witness.WitnessCount != len(publications) {
			return errors.New("native trace suite omits a finite model witness")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFixtures(ctx, runner.Output, publications, bindings)
}

func writeNew(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(contents)
	return errors.Join(writeErr, file.Close())
}

type fixturePackage struct {
	Exports  []string
	Producer string
	Consumer string
}

func testBody(runner, name string) string {
	if runner == "vitest" {
		return "import { expect, test } from 'vitest';\ntest('" + name + "', () => { expect(6 * 7).toBe(42); });\n"
	}
	return "import { test } from 'node:test';\nimport assert from 'node:assert/strict';\ntest('" + name + "', () => { assert.equal(6 * 7, 42); });\n"
}

func writeFixtures(ctx context.Context, output string, cases map[string]state, bindings runnerBindings) error {
	root := filepath.Join(output, "workspace")
	encoded, err := json.Marshal(cases)
	if err != nil {
		return err
	}
	quoted, err := json.Marshal(string(encoded))
	if err != nil {
		return err
	}
	if err := writeNew(filepath.Join(root, "producer_cases", "cases.bzl"), []byte("CASES = json.decode("+string(quoted)+")\n")); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(root, "producer_cases", "BUILD.bazel"), []byte("exports_files([\"cases.bzl\"], visibility = [\"//visibility:public\"])\n")); err != nil {
		return err
	}
	packages := map[string]*fixturePackage{}
	getPackage := func(path []string) *fixturePackage {
		name := strings.Join(path, "/")
		if packages[name] == nil {
			packages[name] = &fixturePackage{}
		}
		return packages[name]
	}
	writeSource := func(pkg, path []string, contents string) error {
		getPackage(pkg).Exports = append(getPackage(pkg).Exports, strings.Join(path[len(pkg):], "/"))
		return writeNew(filepath.Join(root, filepath.Join(path...)), []byte(contents))
	}
	var names []string
	for name := range cases {
		names = append(names, name)
	}
	slices.Sort(names)
	var targets []string
	var runtimeTargets []runtimeTarget
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := cases[name].Input
		getPackage(f.ProducerPackage).Producer = name
		getPackage(f.ConsumerPackage).Consumer = name
		contents := "export const value: number = 42;\n"
		switch f.Kind {
		case "json":
			contents = "{\"value\":42}\n"
		case "asset":
			contents = "producer asset\n"
		case "asset_js":
			contents = "export const value = 42;\n"
		}
		if f.Runner != "" {
			contents = testBody(f.Runner, forwardedBody)
			if err := writeSource(f.ConsumerPackage, f.LocalPath, testBody(f.Runner, localBody)); err != nil {
				return err
			}
		}
		sourcePath := f.SourcePath
		if f.Kind == "generated_js" {
			sourcePath = append(slices.Clone(f.ProducerPackage), "producer.input.js")
		}
		if err := writeSource(f.ProducerPackage, sourcePath, contents); err != nil {
			return err
		}
		if f.Injected {
			if err := writeSource(f.ProducerPackage, append(slices.Clone(f.ProducerPackage), "other.ts"), "export const value: number = 42;\n"); err != nil {
				return err
			}
		}
		if f.Scope {
			if err := writeSource(f.ProducerPackage, append(slices.Clone(f.ProducerPackage), "package.json"), "{\"type\":\"module\"}\n"); err != nil {
				return err
			}
		}
		if f.Shadow {
			if err := writeSource(f.ConsumerPackage, append(slices.Clone(f.ConsumerPackage), "nested", "package.json"), "{\"type\":\"commonjs\"}\n"); err != nil {
				return err
			}
		}
		for _, path := range f.ForeignSources {
			if err := writeSource(path[:len(path)-1], path, "{\"relocated\":true}\n"); err != nil {
				return err
			}
		}
		targets = append(targets, "//"+strings.Join(f.ConsumerPackage, "/")+":replay_test")
		if name == "composed_alias_data" {
			targets = append(targets, "//"+strings.Join(f.ConsumerPackage, "/")+":native_alias_replay_test")
			targets = append(targets, "//"+strings.Join(f.ConsumerPackage, "/")+":runner_alias_replay_test")
		}
		if f.Runner != "" {
			targets = append(targets, "//"+strings.Join(f.ConsumerPackage, "/")+":roots_replay_test")
			runtimeTargets = append(runtimeTargets, runtimeTarget{Label: "//" + strings.Join(f.ConsumerPackage, "/") + ":selected_roots", Runner: f.Runner})
		}
	}
	var packageNames []string
	for name := range packages {
		packageNames = append(packageNames, name)
	}
	slices.Sort(packageNames)
	encodedBindings, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	for _, name := range packageNames {
		p := packages[name]
		var text strings.Builder
		if p.Producer != "" || p.Consumer != "" {
			text.WriteString("load(\"@rules_typescript//ts/private/rules/conformance:replay_assertions.bzl\", \"producer_fixture\", \"consumer_fixture\")\n")
			text.WriteString("load(\"//producer_cases:cases.bzl\", \"CASES\")\n\n")
		}
		if len(p.Exports) > 0 {
			slices.Sort(p.Exports)
			files, err := json.Marshal(slices.Compact(p.Exports))
			if err != nil {
				return err
			}
			fmt.Fprintf(&text, "exports_files(%s, visibility = [\"//visibility:public\"])\n", files)
		}
		for _, call := range []struct{ function, identity string }{{"producer_fixture", p.Producer}, {"consumer_fixture", p.Consumer}} {
			if call.identity != "" {
				name, err := json.Marshal(call.identity)
				if err != nil {
					return err
				}
				fmt.Fprintf(&text, "%s(CASES[%s], runner_bindings = %s)\n", call.function, name, encodedBindings)
			}
		}
		if err := writeNew(filepath.Join(root, filepath.FromSlash(name), "BUILD.bazel"), []byte(text.String())); err != nil {
			return err
		}
	}
	manifest, err := json.MarshalIndent(struct {
		Status         string          `json:"status"`
		Workspace      string          `json:"workspace_fragment"`
		Targets        []string        `json:"analysis_test_targets"`
		ExpectedCount  int             `json:"expected_test_count"`
		RuntimeTargets []runtimeTarget `json:"native_runtime_targets"`
		RuntimeBodies  []string        `json:"required_test_bodies_per_target"`
	}{"exported", root, targets, len(targets), runtimeTargets, []string{forwardedBody, localBody}}, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(filepath.Join(output, "export.json"), append(manifest, '\n'))
}
