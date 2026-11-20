package main

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// typeView copies the files of an npm store tree a type checker reads:
// package.json and other JSON, TypeScript sources, declarations, and
// JavaScript TypeScript tries no declaration in place of, which an untyped
// package's importer resolves to.
func typeView(args []string) error {
	flags := flag.NewFlagSet("typeview", flag.ExitOnError)
	in := flags.String("in", "", "store tree to read")
	out := flags.String("out", "", "directory to write the view to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("typeview needs -in=DIR and -out=DIR")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	return viewDir(*in, *out)
}

func viewDir(src, dest string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dest, entry.Name())
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			err = viewLink(from, to)
		case entry.IsDir():
			err = viewDir(from, to)
		case typeChecked(from):
			if err = os.MkdirAll(dest, 0o755); err == nil {
				err = copyFile(from, to)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// viewLink keeps a link to a directory as the same relative link, its target
// created in the view, and copies a link to a file the view keeps as that file.
// The store tree's links stay inside its package (copyLink).
func viewLink(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	if !info.IsDir() && !typeChecked(from) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(from, to)
	}
	target, err := os.Readlink(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(to), target), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, to)
}

// typeChecked reports whether TypeScript's module resolution can read path:
// a .js or .jsx stands in for its .d.ts, a .mjs for its .d.mts and a .cjs for
// its .d.cts, and no other pair.
func typeChecked(path string) bool {
	name := filepath.Base(path)
	for _, ext := range []string{".ts", ".tsx", ".mts", ".cts", ".json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	stem := strings.TrimSuffix(path, filepath.Ext(path))
	switch filepath.Ext(name) {
	case ".js", ".jsx":
		return !exists(stem + ".d.ts")
	case ".mjs":
		return !exists(stem + ".d.mts")
	case ".cjs":
		return !exists(stem + ".d.cts")
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
