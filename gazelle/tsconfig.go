package typescript

import (
	"path/filepath"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/walk"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

func (s *programStore) resolveCompilerConfig(c *config.Config, cfg string, dirInfo func(string) (walk.DirInfo, error)) (*tsconfig.Resolved, error) {
	return tsconfig.ResolveWithAdmission(filepath.Join(c.RepoRoot, filepath.FromSlash(cfg)), func(absolute string) error {
		rel, err := filepath.Rel(c.RepoRoot, absolute)
		if err != nil || !firstParty(filepath.ToSlash(rel)) {
			return err
		}
		file := filepath.ToSlash(rel)
		if dirInfo != nil {
			s.observeBuild(parentDir(file), dirInfo)
		}
		switch s.metadataIdentity(c, file) {
		case generatedInput:
			return generatedDiscoveryConflict(file)
		case unknownInput:
			return s.incompleteOutput(file)
		}
		return nil
	})
}
