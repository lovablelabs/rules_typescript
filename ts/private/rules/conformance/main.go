package main

import (
	"context"
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
	models := flag.String("models", "", "declared producer model directory")
	output := flag.String("output", "", "fresh evidence and workspace-fragment directory")
	deadline := flag.String("deadline", "", "outer invocation deadline in RFC3339Nano")
	workers := flag.Int("workers", 0, "CPU workers reserved by the outer invocation")
	cleanup := flag.Duration("cleanup", 0, "TLC wait allowance granted by the outer invocation")
	nodeModules := flag.String("node-modules", "", "existing harness node_modules label")
	nodeTypes := flag.String("node-types-dep", "", "existing harness @types/node dependency label")
	vitest := flag.String("vitest-dep", "", "existing harness Vitest dependency label")
	flag.Parse()
	if flag.NArg() != 0 || *workers < 1 || *cleanup <= 0 {
		return errors.New("explicit reserved workers and caller-owned cleanup allowance are required")
	}
	if *nodeModules == "" || *nodeTypes == "" || *vitest == "" {
		return errors.New("existing harness -node-modules, -node-types-dep and -vitest-dep labels are required")
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
	if err := export(ctx, tlatrace.Runner{Java: *java, Jar: *jar, Models: *models, Output: *output, Workers: *workers, Cleanup: *cleanup}, runnerBindings{NodeModules: *nodeModules, NodeTypes: *nodeTypes, Vitest: *vitest}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Println("Producer model checked and fixtures exported; Bazel implementation replay remains with the outer invocation owner.")
	return nil
}
