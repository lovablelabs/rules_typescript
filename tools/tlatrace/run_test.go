package tlatrace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIVerdictRejectsWrongNativeExitAndUnintendedErrors(t *testing.T) {
	positive := "Finished computing initial states: 2 distinct states generated at 2026-09-30 00:00:00.\nModel checking completed. No error has been found.\n6 states generated, 4 distinct states found, 0 states left on queue.\n"
	negative := "Error: Invariant Identity is violated.\nError: The behavior up to this point is:\n"
	for _, tc := range []struct {
		name   string
		c      Case
		code   int
		output string
		ok     bool
	}{
		{"exhaustive-positive", Case{Initial: 2, Distinct: 4, Generated: 6}, 0, positive, true},
		{"native-counterexample", Case{Violations: []string{"Identity"}}, 12, negative, true},
		{"failed-process-is-not-a-counterexample", Case{Violations: []string{"Identity"}}, 1, negative, false},
		{"successful-process-is-not-a-counterexample", Case{Violations: []string{"Identity"}}, 0, negative, false},
		{"extra-error-cannot-qualify", Case{Violations: []string{"Identity"}}, 12, negative + "Error: postcondition failed\n", false},
		{"unintended-invariant-cannot-qualify", Case{Violations: []string{"Other"}}, 12, negative, false},
		{"nonempty-queue-cannot-qualify", Case{Initial: 2, Distinct: 4, Generated: 6}, 0, strings.Replace(positive, "0 states left", "1 states left", 1), false},
		{"missing-completion-cannot-qualify", Case{Initial: 2, Distinct: 4, Generated: 6}, 0, strings.Replace(positive, "Model checking completed. No error has been found.", "", 1), false},
		{"spec-parse-failure-retains-native-diagnostic", Case{Initial: 2, Distinct: 4, Generated: 6}, 150, "Error: Parsing or semantic analysis failed.\nNative diagnostic identifies the failing module and location.\n", false},
		{"exception-module-name-can-exhaust", Case{Module: "Exception", Initial: 2, Distinct: 4, Generated: 6}, 0, "Parsing file /models/Exception.tla\n" + positive, true},
		{"exception-string-is-counterexample-data", Case{Violations: []string{"Identity"}}, 12, negative + "State 1: <Initial predicate>\n/\\ message = \"Exception\"\n", true},
		{"native-exception-exit-cannot-qualify", Case{Initial: 2, Distinct: 4, Generated: 6}, 255, positive + "Exception in thread \"main\" java.lang.IllegalStateException: native failure\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := accept(tc.c, tc.code, tc.output)
			if (err == nil) != tc.ok {
				t.Fatalf("acceptance = %v, want accepted=%t", err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), tc.output) {
				t.Fatalf("failure omitted native diagnostic: %v", err)
			}
		})
	}
}

func TestArtifactMismatchAndAbsentOwnerDeadlineRefuseBeforeTLC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.jar")
	if err := os.WriteFile(path, []byte("retained"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("retained"))
	digest := hex.EncodeToString(sum[:])
	if err := VerifyArtifact(path, digest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(path, digest); err == nil {
		t.Fatal("replaced artifact accepted")
	}
	if _, err := (Runner{}).Run(context.Background(), Case{}); err == nil || !strings.Contains(err.Error(), "aggregate deadline") {
		t.Fatalf("missing owner deadline accepted: %v", err)
	}
}
