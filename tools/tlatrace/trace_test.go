package tlatrace

import (
	"strings"
	"testing"
)

func TestNativeTraceWireRejectsMissingStatesAndConflictingEdges(t *testing.T) {
	const witness = `{"vars":["phase"],"counterexample":{"state":[[2,{"phase":"published"}],[1,{"phase":"assembling"}]],"action":[[[1,{"phase":"assembling"}],{"name":"Publish"},[2,{"phase":"published"}]]]}}`
	for _, tc := range []struct {
		name string
		wire string
		ok   bool
	}{
		{"native-set-order-is-not-trace-order", witness, true},
		{"malformed-json", "{", false},
		{"missing-vars", strings.Replace(witness, `["phase"]`, `[]`, 1), false},
		{"duplicate-state-id", strings.Replace(witness, `"state":[[2,`, `"state":[[1,`, 1), false},
		{"missing-state-id", strings.Replace(witness, `"state":[[2,`, `"state":[[3,`, 1), false},
		{"edge-disagrees-with-state", strings.Replace(witness, `"action":[[[1,{"phase":"assembling"}]`, `"action":[[[1,{"phase":"published"}]`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states, err := Decode([]byte(tc.wire))
			if (err == nil) != tc.ok {
				t.Fatalf("decode = %v, want accepted=%t", err, tc.ok)
			}
			if err == nil && string(states[0]["phase"]) != `"assembling"` {
				t.Fatal("replay used JSON set order instead of native state IDs")
			}
		})
	}
}
