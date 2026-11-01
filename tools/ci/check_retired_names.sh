#!/usr/bin/env bash
# check_retired_names.sh -- no tracked prose or code names a retired attribute,
# kind, provider, directive, export or path. docs/CI_CD.md § Retired Names.

set -euo pipefail

cd "${BUILD_WORKSPACE_DIRECTORY:-$(git rev-parse --show-toplevel)}"

# Whole identifiers. `module_name` is bzlmod's `git_override` keyword and
# `jsx_import_source` an oxc_cli option field, so neither is listed.
WORDS=(
  jsx_mode enable_check tsgo_args
  path_alias_srcs types_srcs vite_types public_globals untyped_packages
  global_setup config_json coverage_thresholds npm_workspace_name
  compiled_tests update_snapshots tsconfig_types
  asset_library css_library css_module json_library
  ts_worker_types ts_bundle vite_bundler ts_npm_publish next_build
  next_dev_server next_serve remix_build svelte_library sveltekit_build
  ts_worker_deploy ts_worker_dry_run ts_worker_dry_run_test
  JsInfo TsDeclarationInfo TsModuleInfo CssInfo CssModuleInfo AssetInfo
  NpmPublishInfo TsLintInfo ts_lint linter_binary TS_TEST_PACKAGE_DIR
  ts_test_macro _ts_auto_node_modules RUNNER_NODE_TEST RUNNER_VITEST
  _generate_tsconfig
  deps_declarations transitive_declarations
  TsStrictDeps _STRICT_DEPS_MJS strict_deps_check
  OxcCompile
  manifest_json member_manifest_json
  _tsaction _copier LAUNCHER_ATTRS
  npmrc_auth
)

# Attribute names that are also fixture directories under tests/: a hit is one
# with no path character on either side.
PATH_CLASHING=(
  compiler_options path_aliases setup_files reads_report strict_deps
)

PATTERNS=(
  'gazelle:ts_(package_boundary|exclude|exclude_dir|ignore|target_name|js_srcs|ambient_types|runtime_dep|warn_unresolved|declarations|path_alias|codegen|npm_hub|npm_mapping|asset_declaration_type)([^[:alnum:]_]|$)'
  'ts/private/ts_(compile|test|lint)\.bzl'
  'strict_deps\.bzl'
  '\.strictdeps'
  'rules/ts-lint\.md'
  '<(name|dirbase)>_lint'
  '\.update_snapshots'
  '<(name|test)>\.reads'
  '_[a-z_]+_test_(compile|node_modules)'
  'member_manifest(_tests)?\.bzl'
  '\._launcher([^_[:alnum:]]|$)'
  '"_launcher": attr'
  'npmrc_auth\.bzl'
  'integration/npmrc_registry'
  'npmrc_registry_runner'
  ':npmrc_registry_test'
  'llvm-resource-directory\.patch'
)

# Files that assert a retired name is absent or inert, with why. Exact in both
# directions: a listed file with no hit is stale.
ALLOWED=$(
  cat <<'ALLOWLIST'
# pins the kinds Gazelle no longer writes
gazelle/kinds_surface_test.go
# asserts no filegroup(tsconfig_types) is written
gazelle/converge_cases_test.go
# asserts types_srcs and tsconfig_types absent from the generated files
tests/integration/runners/gazelle_roundtrip/main.go
# a stale ts_target_name line changes nothing
tests/npm/workspace_link_tests.bzl
ALLOWLIST
)

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf '%s\n' "$ALLOWED" \
  | sed -e 's/#.*//' -e 's/[[:space:]]*$//' \
  | awk 'NF' | LC_ALL=C sort -u > "$work/allowed"

excluded=(
  ':!changelog.d' ':!CHANGELOG.md' ':!TODO.md' ':!rules-ts-v2-project-plan.md'
  ':!tools/ci/check_retired_names.sh' ':!*pnpm-lock.yaml' ':!*.snap'
)
words="$(IFS='|'; echo "${WORDS[*]}")"
clashing="$(IFS='|'; echo "${PATH_CLASHING[*]}")"
patterns=()
for p in "${PATTERNS[@]}"; do patterns+=(-e "$p"); done

{
  git grep -n -I -w -E -e "$words" -- . \
    "${excluded[@]}" || true
  git grep -n -I -E \
    -e "(^|[^/.[:alnum:]_-])(${clashing})($|[^/.[:alnum:]_-])" \
    "${patterns[@]}" -- . "${excluded[@]}" || true
} | grep -v -E '^docs/gazelle/directives\.md:[0-9]+:\| `# gazelle:ts_' \
  | LC_ALL=C sort -u > "$work/hits" || true

cut -d: -f1 "$work/hits" | LC_ALL=C sort -u > "$work/hit_files"

status=0

LC_ALL=C comm -23 "$work/hit_files" "$work/allowed" > "$work/offending_files"
if [ -s "$work/offending_files" ]; then
  awk -F: -v files="$(cat "$work/offending_files")" '
      BEGIN { n = split(files, f, "\n"); for (i = 1; i <= n; i++) ok[f[i]] = 1 }
      ok[$1]' "$work/hits" > "$work/offending"
  cat >&2 <<MESSAGE
check_retired_names: these lines name a retired attribute, kind, provider,
directive, export or path:

$(sed 's/^/  /' "$work/offending")

Rewrite each from the end state. A file that asserts the name is absent or
inert goes in ALLOWED in this script, with why.
MESSAGE
  status=1
fi

LC_ALL=C comm -13 "$work/hit_files" "$work/allowed" > "$work/stale"
if [ -s "$work/stale" ]; then
  cat >&2 <<MESSAGE
check_retired_names: these ALLOWED entries are stale -- each file no longer
names a retired name, or no longer exists:

$(sed 's/^/  /' "$work/stale")

Remove them from ALLOWED in this script.
MESSAGE
  status=1
fi

if [ "$status" -ne 0 ]; then
  exit "$status"
fi

printf 'check_retired_names: no retired name outside the changelog'
printf ' (%s files assert an absence, allowlisted).\n' \
  "$(wc -l < "$work/allowed" | tr -d ' ')"
