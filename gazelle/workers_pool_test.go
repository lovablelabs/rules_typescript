package typescript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/rule"
)

// One file in the config's package, whatever the quotes or the name; none,
// two, an escape or a missing file is nothing, said once.
func TestWranglerConfigOf(t *testing.T) {
	root := t.TempDir()
	writeWorkspace(t, root, map[string]string{
		"w/wrangler.jsonc":       "{}\n",
		"w/wrangler.test.jsonc":  "{}\n",
		"w/config/wrangler.toml": "\n",
		"w/sub/tsconfig.json":    includeTs,
		"w/sub/wrangler.json":    "{}\n",
		"w/double.mts":           "'./wrangler.jsonc'; `wrangler.toml`\n",
		"w/single.mts":           `configPath: "./wrangler.jsonc"` + "\n",
		"w/twice.mts":            `"./wrangler.jsonc"; "wrangler.jsonc"` + "\n",
		"w/named.mts":            `configPath: './wrangler.test.jsonc'` + "\n",
		"w/below.mts":            "`config/wrangler.toml`\n",
		"w/deeper.mts":           `"sub/wrangler.json"` + "\n",
		"w/above.mts":            `"../wrangler.jsonc"` + "\n",
		"w/absent.mts":           `"./wrangler.staging.jsonc"` + "\n",
		"w/none.mts":             "export default {}\n",
		"w/comment.mts":          "// the wrangler.jsonc beside this file\n",
		"wrangler.jsonc":         "{}\n",
		"root.mts":               `"wrangler.jsonc"` + "\n",
	})
	configuration := emptyConfig()
	configuration.RepoRoot = root
	s := getConfig(configuration).programs
	s.visit("", []string{"wrangler.jsonc"})
	s.visit("w", []string{"wrangler.jsonc", "wrangler.test.jsonc", "config/wrangler.toml"})
	s.visit("w/sub", []string{"wrangler.json"})
	s.packages["w"] = map[string]bool{"w/a.ts": true}
	s.packages["w/sub"] = map[string]bool{"w/sub/b.ts": true}
	for _, c := range []struct{ cfg, want, said string }{
		{"w/single.mts", "w/wrangler.jsonc", ""},
		{"w/twice.mts", "w/wrangler.jsonc", ""},
		{"w/named.mts", "w/wrangler.test.jsonc", ""},
		{"w/below.mts", "w/config/wrangler.toml", ""},
		{"root.mts", "wrangler.jsonc", ""},
		{"w/none.mts", "", ""},
		{"w/comment.mts", "", ""},
		{"w/double.mts", "", "names 2 wrangler configs"},
		{"w/above.mts", "", "../wrangler.jsonc, which is not in w"},
		{"w/deeper.mts", "", "sub/wrangler.json, which is not in w"},
		{"w/absent.mts", "", "w/wrangler.staging.jsonc, which is not there"},
	} {
		var got string
		logged := captureLog(t, func() {
			for range 2 {
				var err error
				got, err = s.wranglerConfigOf(configuration, c.cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.cfg, got, c.want)
		}
		if c.said == "" && logged != "" {
			t.Errorf("%s: said %q, want nothing", c.cfg, logged)
		}
		if c.said != "" && (!strings.Contains(logged, c.said) ||
			strings.Count(logged, "\n") != 1) {
			t.Errorf("%s: log, want one line with %q:\n%s", c.cfg, c.said, logged)
		}
	}
	if got, err := s.wranglerConfigOf(configuration, filepath.Join("w", "gone.mts")); got != "" || err != nil {
		t.Errorf("an unreadable config names %q, error %v", got, err)
	}
}

func TestWranglerInputCopiesCannotChangeDeclaredProvenance(t *testing.T) {
	for _, input := range []struct{ name, declaration, file, want, wantError, diagnostic string }{
		{"scalar", `genrule(name = "config", outs = ["wrangler.jsonc"])`, "wrangler.jsonc", "app/wrangler.jsonc", "", ""},
		{"unknown", `genrule(name = "config", outs = OUTPUTS)`, "wrangler.jsonc", "", "cannot determine output provenance", ""},
		{"tree", `ts_codegen(name = "config", out_dir = "generated")`, "generated/wrangler.jsonc", "", "individual output File", ""},
		{"excluded authored", "", "wrangler.jsonc", "", "", "staged as wrangler_config"},
	} {
		t.Run(input.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "app/vitest.config.mts"), `export default { configPath: "./`+input.file+`" };`)
			for _, state := range []string{"cold", "materialized", "removed"} {
				file := filepath.Join(root, "app", input.file)
				if state == "materialized" {
					writeFile(t, file, "stale contents must not select an input\n")
				} else if state == "removed" {
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
				}
				c := emptyConfig()
				c.RepoRoot = root
				s := getConfig(c).programs
				f, err := rule.LoadData("app/BUILD.bazel", "app", []byte(input.declaration))
				if err != nil {
					t.Fatal(err)
				}
				s.recordBuild(c, "app", f)
				s.packages["app"] = map[string]bool{"app/index.ts": true}
				if state == "materialized" && input.name != "excluded authored" {
					s.visit("app", []string{input.file})
				}
				var got string
				logged := captureLog(t, func() {
					for range 2 {
						got, err = s.wranglerConfigOf(c, "app/vitest.config.mts")
						if input.wantError == "" && err != nil || input.wantError != "" && (err == nil || !strings.Contains(err.Error(), input.wantError)) {
							t.Errorf("%s Wrangler error = %v, want %q", state, err, input.wantError)
						}
					}
				})
				if got != input.want || !strings.Contains(logged, input.diagnostic) || input.diagnostic == "" && logged != "" {
					t.Errorf("%s Wrangler identity = %q, log %q; want %q, diagnostic %q", state, got, logged, input.want, input.diagnostic)
				}
			}
		})
	}
}
