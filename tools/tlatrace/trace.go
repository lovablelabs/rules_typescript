package tlatrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
)

type State map[string]json.RawMessage

func Decode(data []byte) ([]State, error) {
	var dump struct {
		Variables      []string `json:"vars"`
		Counterexample struct {
			States  []json.RawMessage   `json:"state"`
			Actions [][]json.RawMessage `json:"action"`
		} `json:"counterexample"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, err
	}
	n := len(dump.Counterexample.States)
	if n == 0 || len(dump.Variables) == 0 || len(dump.Counterexample.Actions) != n-1 {
		return nil, errors.New("native TLC witness must be a nonempty finite path")
	}
	variables := slices.Clone(dump.Variables)
	slices.Sort(variables)
	if len(slices.Compact(variables)) != len(dump.Variables) {
		return nil, errors.New("native TLC witness repeats a variable")
	}
	states := make([]State, n)
	for _, raw := range dump.Counterexample.States {
		id, state, err := decodeNode(raw)
		if err != nil || id < 1 || id > n || states[id-1] != nil {
			return nil, errors.New("native TLC witness has an invalid or repeated state ID")
		}
		if len(state) != len(variables) {
			return nil, errors.New("native TLC witness has incomplete state variables")
		}
		for _, key := range variables {
			if _, ok := state[key]; !ok {
				return nil, fmt.Errorf("native TLC witness omits %q", key)
			}
		}
		states[id-1] = state
	}
	edges := make([]bool, n-1)
	for _, edge := range dump.Counterexample.Actions {
		if len(edge) != 3 {
			return nil, errors.New("native TLC witness has a malformed action")
		}
		from, source, sourceErr := decodeNode(edge[0])
		to, target, targetErr := decodeNode(edge[2])
		if sourceErr != nil || targetErr != nil || from < 1 || from >= n || to != from+1 || edges[from-1] {
			return nil, errors.New("native TLC witness has an invalid or repeated edge")
		}
		if !reflect.DeepEqual(source, states[from-1]) || !reflect.DeepEqual(target, states[to-1]) {
			return nil, errors.New("native TLC witness edge disagrees with its state")
		}
		edges[from-1] = true
	}
	return states, nil
}

func decodeNode(raw json.RawMessage) (int, State, error) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return 0, nil, errors.New("native TLC state must be an ID and a record")
	}
	var id int
	var state State
	if err := json.Unmarshal(tuple[0], &id); err != nil {
		return 0, nil, err
	}
	if err := json.Unmarshal(tuple[1], &state); err != nil || len(state) == 0 {
		return 0, nil, errors.New("native TLC state has no variables")
	}
	return id, state, nil
}
