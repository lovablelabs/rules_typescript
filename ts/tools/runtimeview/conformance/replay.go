package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/mikn/rules_typescript/tools/tlatrace"
	"github.com/mikn/rules_typescript/ts/tools/runtimeview"
)

type state struct {
	Phase    string    `json:"phase"`
	Renderer string    `json:"renderer"`
	Bindings []binding `json:"bindings"`
}

type binding struct {
	Coordinate string `json:"coordinate"`
	Artifact   string `json:"artifact"`
	Canonical  bool   `json:"canonical"`
}

func replay(trace []tlatrace.State, directory string) error {
	if len(trace) != 2 {
		return errors.New("canonical trace must contain initialization and publication")
	}
	states := make([]state, len(trace))
	for i, raw := range trace {
		data, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&states[i]); err != nil {
			return err
		}
	}
	initial, final := states[0], states[1]
	if initial.Phase != "assembling" || (final.Phase != "published" && final.Phase != "rejected") || len(initial.Bindings) == 0 {
		return errors.New("canonical trace has no valid publication transition")
	}
	initial.Phase = final.Phase
	if !reflect.DeepEqual(initial, final) {
		return errors.New("canonical trace changes its input facts during publication")
	}
	root := filepath.Join(directory, "view")
	if err := os.MkdirAll(filepath.Join(directory, "inputs"), 0o755); err != nil {
		return err
	}
	entries := map[string]string{}
	artifacts := map[string]string{}
	var modules []string
	var inputs []runtimeview.Input
	for _, mapping := range initial.Bindings {
		name, artifact := mapping.Coordinate, mapping.Artifact
		if !component(name) || !component(artifact) {
			return errors.New("canonical trace contains a nonlocal identity")
		}
		if _, ok := artifacts[artifact]; !ok {
			path := filepath.Join(directory, "inputs", artifact+".js")
			if err := os.WriteFile(path, []byte("export const value = 1;\n"), 0o644); err != nil {
				return err
			}
			artifacts[artifact] = path
			inputs = append(inputs, runtimeview.Input{Path: path, Output: filepath.Join(directory, "copies", artifact+".js"), Kind: "file"})
		}
		coordinate := "_main/" + name + "/index.js"
		if _, ok := entries[coordinate]; ok {
			return errors.New("canonical trace repeats a coordinate")
		}
		entries[coordinate] = artifacts[artifact]
		if mapping.Canonical {
			modules = append(modules, coordinate)
		}
	}
	if len(modules) == 0 {
		return errors.New("canonical trace declares no modules")
	}
	var err error
	switch initial.Renderer {
	case "stage":
		err = runtimeview.Stage(root, entries, modules)
	case "build":
		err = runtimeview.Build(runtimeview.Spec{Root: root, Inputs: inputs, Entries: entries, Modules: modules})
	default:
		return fmt.Errorf("unknown model renderer %q", initial.Renderer)
	}
	if final.Phase == "rejected" {
		if err == nil || !strings.Contains(err.Error(), "module artifact") || !strings.Contains(err.Error(), "multiple paths") {
			return fmt.Errorf("model rejected canonical publication, renderer returned %v", err)
		}
		for _, module := range modules {
			info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(module)))
			if statErr == nil && info.Mode().IsRegular() {
				return fmt.Errorf("rejected module %q was materialized", module)
			}
			if statErr != nil && !os.IsNotExist(statErr) {
				return statErr
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("model admitted canonical publication: %w", err)
	}
	for _, module := range modules {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(module)))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("admitted module %q was not materialized: %v", module, err)
		}
	}
	return nil
}

func component(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\")
}
