package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mikn/rules_typescript/tools/tlatrace"
)

const tlcSHA256 = "c953c663948041f6b198935952233aacc09bef81784c929acb26eae07bedc72c"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	java := flag.String("java", "", "declared Java executable")
	jar := flag.String("tlc", "", "declared pinned TLC jar")
	models := flag.String("models", "", "RunfilesIdentity model directory")
	output := flag.String("output", "", "new evidence directory")
	deadline := flag.String("deadline", "", "outer invocation deadline in RFC3339Nano")
	workers := flag.Int("workers", 0, "CPU workers reserved by the outer invocation")
	cleanup := flag.Duration("cleanup", 0, "TLC wait allowance granted by the outer invocation")
	flag.Parse()
	if flag.NArg() != 0 || *workers < 1 || *cleanup <= 0 {
		return errors.New("explicit reserved workers and caller-owned cleanup allowance are required")
	}
	end, err := time.Parse(time.RFC3339Nano, *deadline)
	if err != nil || !end.After(time.Now()) {
		return errors.New("an unexpired outer invocation deadline is required")
	}
	for _, path := range []*string{java, jar, models, output} {
		if *path == "" {
			return errors.New("explicit -java, -tlc, -models and -output inputs are required")
		}
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		*path = absolute
	}
	interrupt, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithDeadline(interrupt, end)
	defer cancel()
	if err := tlatrace.VerifyArtifact(*jar, tlcSHA256); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(*models, "suite.json"))
	if err != nil {
		return err
	}
	var cases []tlatrace.Case
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&cases)
	file.Close()
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return errors.New("the model suite is empty")
	}
	if err := os.Mkdir(*output, 0o755); err != nil {
		return err
	}
	receipts, err := os.OpenFile(filepath.Join(*output, "receipts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer receipts.Close()
	runner := tlatrace.Runner{Java: *java, Jar: *jar, Models: *models, Output: *output, Workers: *workers, Cleanup: *cleanup}
	for _, c := range cases {
		started := time.Now()
		states, err := runner.Run(ctx, c)
		if err == nil && c.Replay {
			err = replay(states, filepath.Join(*output, c.Config+".replay"))
		}
		if err == nil {
			err = ctx.Err()
		}
		status := "accepted"
		if err != nil {
			status = err.Error()
		}
		if writeErr := json.NewEncoder(receipts).Encode(struct {
			Case    tlatrace.Case `json:"case"`
			Status  string        `json:"status"`
			Seconds float64       `json:"seconds"`
		}{c, status, time.Since(started).Seconds()}); writeErr != nil {
			return writeErr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", c.Config, err)
		}
	}
	fmt.Printf("Verified %d model cases, including canonical Build/Stage trace replay.\n", len(cases))
	return nil
}
