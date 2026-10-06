// Command copy_to_workspace writes files a Bazel rule declared into the source
// tree. It is the whole of `bazel run //:refresh_tsconfig`: what to generate is
// decided at analysis time, and this only puts the result where an editor looks.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

type entry struct {
	Rlocation  string `json:"rlocation"`
	Dest       string `json:"dest"`
	OutputRoot string `json:"output_root,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "copy_to_workspace: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	workspace := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspace == "" {
		return fmt.Errorf("BUILD_WORKSPACE_DIRECTORY is unset; run this through `bazel run`")
	}
	manifestPath := os.Getenv("COPY_TO_WORKSPACE_MANIFEST")
	if manifestPath == "" {
		return fmt.Errorf("COPY_TO_WORKSPACE_MANIFEST is unset; the refresh_workspace_files rule sets it")
	}

	files, err := runfiles.New()
	if err != nil {
		return err
	}
	raw, err := read(files, manifestPath)
	if err != nil {
		return err
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("parsing %s: %w", manifestPath, err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("resolving workspace %s: %w", workspace, err)
	}
	workspace = canonicalWorkspace

	for _, e := range entries {
		artifact, err := files.Rlocation(e.Rlocation)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(artifact)
		if err != nil {
			return err
		}
		dest, err := resolve(workspace, e.Dest)
		if err != nil {
			return err
		}
		if e.OutputRoot != "" {
			root, err := resolve(workspace, e.OutputRoot)
			if err != nil {
				return err
			}
			canonical, err := filepath.EvalSymlinks(artifact)
			if err != nil {
				return err
			}
			_, shortPath, _ := strings.Cut(e.Rlocation, "/")
			suffix := "/" + shortPath
			if shortPath == "" || !strings.HasSuffix(filepath.ToSlash(canonical), suffix) {
				return fmt.Errorf("editor runfile %s does not retain its output-relative path", e.Rlocation)
			}
			canonicalRoot := filepath.FromSlash(strings.TrimSuffix(filepath.ToSlash(canonical), suffix))
			content, err = canonicalEditorPaths(content, filepath.Dir(dest), root, canonicalRoot)
			if err != nil {
				return fmt.Errorf("binding output paths for %s: %w", e.Dest, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := publish(dest, content); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", e.Dest)
	}
	return nil
}

func publish(dest string, content []byte) error {
	mode := os.FileMode(0o644)
	info, err := os.Lstat(dest)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(dest)
			if os.IsNotExist(err) {
				target, err = os.Readlink(dest)
				if err == nil && !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(dest), target)
				}
			}
			if err != nil {
				return err
			}
			return publish(target, content)
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	name := filepath.Join(filepath.Dir(dest), ".copy-to-workspace-"+rand.Text())
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(name)
	defer file.Close()
	if info != nil {
		if err := file.Chmod(mode); err != nil {
			return err
		}
	}
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, dest)
}

func canonicalEditorPaths(content []byte, directory, root, canonical string) ([]byte, error) {
	// Native watchers classify lexical paths, so a workspace symlink hides the output root.
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var config map[string]any
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	sources, ok := config["rootDirSources"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid editor project: rootDirSources must be an array")
	}
	var relocate func(any) any
	relocate = func(value any) any {
		switch value := value.(type) {
		case []any:
			for i, item := range value {
				value[i] = relocate(item)
			}
		case string:
			target := filepath.FromSlash(value)
			if !filepath.IsAbs(target) {
				target = filepath.Join(directory, target)
			}
			rel, err := filepath.Rel(root, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return value
			}
			return filepath.ToSlash(filepath.Join(canonical, rel))
		}
		return value
	}
	for _, key := range []string{"extends", "files", "include", "exclude"} {
		if value, ok := config[key]; ok {
			config[key] = relocate(value)
		}
	}
	options, _ := config["compilerOptions"].(map[string]any)
	for _, key := range []string{"rootDir", "rootDirs", "typeRoots", "declarationDir"} {
		if value, ok := options[key]; ok {
			options[key] = relocate(value)
		}
	}
	if value, ok := options["rootDir"].(string); ok && value != "" {
		directoryRoot := filepath.FromSlash(value)
		if !filepath.IsAbs(directoryRoot) {
			directoryRoot = filepath.Join(directory, directoryRoot)
		}
		originalRoot := directoryRoot
		for _, source := range sources {
			value, ok := source.(string)
			if !ok || value == "" {
				return nil, fmt.Errorf("invalid compiler rootDir containment source %v", source)
			}
			target := filepath.FromSlash(relocate(value).(string))
			if !filepath.IsAbs(target) {
				target = filepath.Join(directory, target)
			}
			rel, err := filepath.Rel(directoryRoot, filepath.Dir(target))
			if err != nil {
				return nil, fmt.Errorf("finding editor root for %s and %s: %w", directoryRoot, target, err)
			}
			for _, component := range strings.Split(rel, string(filepath.Separator)) {
				if component != ".." {
					break
				}
				directoryRoot = filepath.Dir(directoryRoot)
			}
		}
		outputMappingDisabled := true
		for _, key := range []string{"outDir", "declarationDir"} {
			if value, known := options[key]; !known || value != nil {
				outputMappingDisabled = false
				if directoryRoot != originalRoot {
					return nil, fmt.Errorf("cannot widen rootDir %s to contain generated sources under %s while compilerOptions.%s is set or inherited; use a generated editor project without emission output directories or an authored editor project without generated_sources", originalRoot, directoryRoot, key)
				}
			}
		}
		if len(sources) > 0 && outputMappingDisabled {
			// TypeScript checks lexical paths, and a workspace alias can live anywhere on this volume.
			options["rootDir"] = filepath.ToSlash(filepath.VolumeName(directoryRoot) + string(filepath.Separator))
		}
	}
	delete(config, "rootDirSources")
	var authored tsconfig.File
	if err := json.Unmarshal(content, &authored); err != nil {
		return nil, err
	}
	if paths := authored.CompilerOptions.Paths; paths != nil {
		for _, values := range paths.Entries() {
			for i, value := range values {
				values[i] = relocate(value).(string)
			}
		}
		options["paths"] = paths
	}
	bound, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(bound, '\n'), nil
}

func read(files *runfiles.Runfiles, rlocation string) ([]byte, error) {
	path, err := files.Rlocation(rlocation)
	if err != nil {
		return nil, fmt.Errorf("locating %s: %w", rlocation, err)
	}
	return os.ReadFile(path)
}

func resolve(workspace, dest string) (string, error) {
	// A dest that escapes the workspace would write wherever the rule pleased.
	path := filepath.Join(workspace, filepath.FromSlash(dest))
	rel, err := filepath.Rel(workspace, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("destination %q is outside the workspace", dest)
	}
	return path, nil
}
