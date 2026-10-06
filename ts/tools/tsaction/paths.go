// The paths step writes the chain's `paths` for a ts_test's generated vitest
// config, which resolves an alias at run time as tsgo did at compile time.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// pathsFile is what the generated vitest config reads: the user's map, and the
// directory its values resolve against relative to the test's package.
type pathsFile struct {
	Dir   string          `json:"dir"`
	Paths *tsconfig.Paths `json:"paths"`
}

func writePaths(args []string) error {
	var project, pkg, binDir, out string
	flags := flag.NewFlagSet("paths", flag.ExitOnError)
	flags.StringVar(&project, "tsconfig", "", "the test's tsconfig.json")
	flags.StringVar(&pkg, "package", "",
		"the test's package, which dir is written relative to")
	flags.StringVar(&binDir, "bin_dir", "", "the output tree's root")
	flags.StringVar(&out, "out", "", "the JSON to write")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if project == "" || out == "" {
		return errors.New("paths: -tsconfig and -out are required")
	}
	chain, err := tsconfig.Resolve(project)
	if err != nil {
		return err
	}
	file := pathsFile{Paths: &tsconfig.Paths{}}
	if chain.Paths != nil {
		dir := strings.TrimPrefix(filepath.ToSlash(chain.PathsDir), binDir+"/")
		rel, err := filepath.Rel(filepath.FromSlash(pkg), filepath.FromSlash(dir))
		if err != nil {
			return err
		}
		file.Dir = filepath.ToSlash(rel)
		file.Paths = chain.Paths
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(data, '\n'), 0o644)
}
