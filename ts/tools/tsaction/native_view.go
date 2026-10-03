package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/mikn/rules_typescript/ts/tools/runtimeview"
)

func nativeView(args []string) error {
	flags := flag.NewFlagSet("native-view", flag.ContinueOnError)
	specPath := flags.String("spec", "", "declared runtime view inputs and outputs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *specPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("native-view requires -spec=FILE")
	}
	file, err := os.Open(*specPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var spec runtimeview.Spec
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return err
	}
	return runtimeview.Build(spec)
}
