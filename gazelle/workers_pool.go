package typescript

import (
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
)

// The Workers pool's half of Gazelle, the one tool-specific file: called from
// generate.go and resolve.go; docs/gazelle/overview.md § A Workers-Pool Config.

const (
	wranglerConfigTargetName = "wrangler_config"
	workersPoolPackage       = "@cloudflare/vitest-pool-workers"
	istanbulPackage          = "@vitest/coverage-istanbul"
)

// A string literal naming a wrangler config: `wrangler.configPath` as written.
var wranglerLiteral = regexp.MustCompile(
	"[\"'`]([^\"'`]*wrangler[\\w.-]*\\.(?:jsonc|json|toml))[\"'`]")

func (s *programStore) wranglerConfigOf(c *config.Config, cfg string) (string, error) {
	if s.generatedFile(cfg, true) {
		return "", nil
	}
	if file, done := s.wranglerConfigs[cfg]; done {
		return file, nil
	}
	file := ""
	switch names := wranglerLiterals(filepath.Join(c.RepoRoot, cfg)); {
	case len(names) == 0:
	case len(names) > 1:
		log.Printf("typescript: %s names %d wrangler configs (%s), so nothing "+
			"is staged as wrangler_config", cfg, len(names),
			strings.Join(names, ", "))
	default:
		var err error
		file, err = s.wranglerConfigIn(c, cfg, names[0])
		if err != nil {
			return "", err
		}
	}
	s.wranglerConfigs[cfg] = file
	return file, nil
}

// wranglerLiterals is every distinct wrangler config the file names, each as
// first spelled.
func wranglerLiterals(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out, seen []string
	for _, m := range wranglerLiteral.FindAllSubmatch(data, -1) {
		lit := string(m[1])
		if clean := path.Clean(lit); !slices.Contains(seen, clean) {
			seen = append(seen, clean)
			out = append(out, lit)
		}
	}
	return out
}

func (s *programStore) observeWranglerConfig(c *config.Config, cfg string) {
	if s.generatedFile(cfg, true) {
		return
	}
	names := wranglerLiterals(filepath.Join(c.RepoRoot, cfg))
	if len(names) == 1 {
		file := path.Join(parentDir(cfg), names[0])
		if firstParty(file) {
			s.observeBuild(parentDir(file), walk.GetDirInfo)
		}
	}
}

func (s *programStore) wranglerConfigIn(c *config.Config, cfg, literal string) (string, error) {
	dir := parentDir(cfg)
	file := path.Join(dir, literal)
	if !firstParty(file) || !dirIsAncestorOf(dir, file) ||
		s.nearestPackage(parentDir(file)) != dir {
		log.Printf("typescript: %s names %s, which is not in %s, so nothing is "+
			"staged as wrangler_config", cfg, literal, orRepoRoot(dir))
		return "", nil
	}
	switch s.inputIdentity(c, s.index, file) {
	case unknownInput:
		return "", s.incompleteOutput(file)
	case generatedInput:
		if producer, _ := s.outputProducer(file); producer == nil {
			return "", fmt.Errorf("typescript: %s names %s, which has no individual output File; "+
				"declare a scalar output to stage it as wrangler_config", cfg, file)
		}
	case unavailableInput:
		log.Printf("typescript: %s names %s, which is not there or is excluded, so nothing is "+
			"staged as wrangler_config", cfg, file)
		return "", nil
	}
	return file, nil
}

// wranglerConfigRule is the filegroup over the wrangler config the exported
// vitest config at cfg names; nil when it names none.
func wranglerConfigRule(args language.GenerateArgs, tc *tsConfig,
	cfg string) *rule.Rule {
	r := rule.NewRule("filegroup", wranglerConfigTargetName)
	if kept := existingRule(args, "filegroup", wranglerConfigTargetName); kept != nil &&
		(kept.ShouldKeep() || attrKept(kept, "srcs")) {
		if srcs := kept.Attr("srcs"); srcs != nil {
			r.SetAttr("srcs", srcs)
		}
	} else {
		tc.programs.observeWranglerConfig(args.Config, cfg)
		file, err := tc.programs.wranglerConfigOf(args.Config, cfg)
		if err != nil {
			log.Fatal(err)
		}
		if file == "" {
			return nil
		}
		r.SetAttr("srcs", []string{wranglerConfigLabel(tc, file, cfg, args.Rel)})
	}
	r.SetAttr("visibility", []string{"//visibility:public"})
	return r
}

func importsWorkersPool(edges []explainfiles.Edge) bool {
	for _, e := range edges {
		if e.Kind == explainfiles.Import && isBareSpecifier(e.Specifier) &&
			barePackageName(e.Specifier) == workersPoolPackage {
			return true
		}
	}
	return false
}

func workersPoolImport(c *config.Config, tc *tsConfig, r *rule.Rule, cfg string,
	e explainfiles.Edge, importer string, from label.Label) bool {
	if !importsWorkersPool([]explainfiles.Edge{e}) {
		return false
	}
	effective := tc.programs.semanticRule(emissionLabel(c.RepoName, from.Pkg, ":"+from.Name))
	if effective == nil {
		effective = r
	}
	if effective.ShouldKeep() {
		return false
	}
	wrangler := effective.AttrString("wrangler_config")
	if !attrKept(effective, "wrangler_config") {
		wrangler = ""
		file, err := tc.programs.wranglerConfigOf(c, cfg)
		if err != nil {
			log.Fatal(err)
		}
		if file != "" {
			wrangler = wranglerConfigLabel(tc, file, cfg, from.Pkg)
			r.SetAttr("wrangler_config", wrangler)
		}
	}
	if wrangler != "" && !attrKept(effective, "workers_pool") {
		if previous := r.AttrString("workers_pool"); previous != "" && previous != importer {
			resolution := workersPoolResolution(tc.lock, previous, from)
			if resolution == "" || resolution != workersPoolResolution(tc.lock, importer, from) {
				log.Fatalf("typescript: %s: config imports Workers pools from both %s and %s with different resolutions; one wrangler_config needs one pool resolution. Did you mean to split these configs into separate ts_test targets?", from, previous, importer)
			}
			if previous < importer {
				importer = previous
			}
		}
		r.SetAttr("workers_pool", importer)
	}
	return true
}

func workersPoolResolution(lock *npmLock, owner string, from label.Label) string {
	if lock == nil {
		return ""
	}
	importer, err := label.Parse(owner)
	if err != nil {
		return ""
	}
	entry := lock.importers[importer.Abs(from.Repo, from.Pkg).Pkg]
	if entry == nil {
		return ""
	}
	if member := entry.links[workersPoolPackage]; member != "" {
		return "link:" + member
	}
	return entry.deps[workersPoolPackage]
}

func workersPoolCoverage(tc *tsConfig, r *rule.Rule, cfg string, from label.Label) string {
	available := false
	if tc.lock != nil {
		_, available = tc.lock.declaring(istanbulPackage, from.Pkg)
	}
	if !available {
		log.Printf("typescript: %s: %s runs the Workers pool, which refuses v8 "+
			"coverage, and no importer above %s declares %s; no coverage_provider, so bazel "+
			"coverage fails on it", from, cfg, orRepoRoot(from.Pkg), istanbulPackage)
		return ""
	}
	r.SetAttr("coverage_provider", "istanbul")
	return tc.lock.label(istanbulPackage, from.Pkg)
}

// The file by name from the config's own package, else the owner's filegroup.
func wranglerConfigLabel(tc *tsConfig, file, cfg, pkg string) string {
	if dir := parentDir(cfg); dir != pkg {
		return "//" + dir + ":" + wranglerConfigTargetName
	}
	owner := configSrcPackage(tc, file, pkg)
	name := strings.TrimPrefix(file, owner+"/")
	if owner != pkg {
		return "//" + owner + ":" + name
	}
	if source, ok := srcLabel(name); ok {
		return source
	}
	log.Fatalf("typescript: %s names %s, which cannot be a Bazel file label; rename the Wrangler config", cfg, file)
	return ""
}
