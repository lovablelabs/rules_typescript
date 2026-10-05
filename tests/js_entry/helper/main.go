package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func lookup() error {
	rf, err := runfiles.New(runfiles.SourceRepo(""))
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(os.Args)-1)
	for _, key := range os.Args[1:] {
		path, err := rf.Rlocation(key)
		if err != nil {
			return err
		}
		paths = append(paths, path)
	}
	return json.NewEncoder(os.Stdout).Encode(paths)
}

func main() {
	if err := lookup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
