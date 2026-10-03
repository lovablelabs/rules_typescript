package main

import (
	"fmt"
	"os"
)

func main() { os.Exit(run()) }

// run is main's body, separated so it can return an exit status instead of
// calling os.Exit from every error path.
func run() int {
	args := os.Args[1:]
	dump := os.Getenv(DumpEnvVar) != ""
	if len(args) > 0 && args[0] == DumpFlag {
		dump = true
		args = args[1:]
	}

	resolver, resolverErr := NewResolver()
	cfg, cfgPath, err := LoadConfig(os.Args[0], resolver)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if resolverErr != nil {
			fmt.Fprintln(os.Stderr, resolverErr)
		}
		return 1
	}
	if resolverErr != nil {
		fmt.Fprintf(os.Stderr, "%v (target %s)\n", resolverErr, cfg.Label)
		return 1
	}
	shard := Shard{Total: 1}
	if cfg.Mode == ModeVitest || cfg.Mode == ModeNodeTest {
		shard, err = parseShard(os.Getenv("TEST_SHARD_INDEX"), os.Getenv("TEST_TOTAL_SHARDS"))
		if err == nil && !dump {
			err = advertiseSharding(os.Getenv("TEST_SHARD_STATUS_FILE"))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	plan, err := MakePlan(cfg, resolver, args, shard)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if dump {
		defer plan.Cleanup()
		if err := Dump(plan, cfgPath, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	code, err := Run(plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return code
}
