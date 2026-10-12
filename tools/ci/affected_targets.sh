#!/usr/bin/env bash
# affected_targets.sh -- write the targets `bazel test --config=ci` has to run
# for the change BASE..HEAD, one label per line, or the single pattern `//...`
# when the change can reach a target the build graph does not show.
#
# The answer is `rdeps(//..., <changed files>)`, which is exact only for an
# input the graph names. Everything else falls back to `//...`:
#
#   - a deleted file: a glob that matched it changes no BUILD file;
#   - a BUILD file, a .bzl file, MODULE.bazel, a .bazelrc or .bazelversion:
#     they change the graph itself, not a node in it;
#   - a lockfile, a manifest or a patch, and any file MODULE.bazel names by
#     label: module extensions and repository rules read them, and nothing in
#     //... depends on a repository rule's inputs;
#   - .github/, tools/, platforms/ and ts/toolchain/: the workflow and its
#     scripts, and what every action resolves;
#   - a file inside the closure of a toolchain this workspace registers: a
#     toolchain is resolved at analysis, so the query graph has no edge from a
#     rule that uses it to the toolchain's sources;
#   - a file no target owns, unless it is documentation.
#
# e2e/ and examples/ are workspaces of their own, in .bazelignore, so nothing in
# //... can read them; their jobs run them whole on every event.
#
# The nested-Bazel tests read the source tree outside their declared inputs, so
# rdeps cannot see what they depend on. The fast lane drops them anyway, and the
# integration-tests job runs them in full on every event.
#
#   tools/ci/affected_targets.sh BASE OUT
#   BAZEL=bazelisk tools/ci/affected_targets.sh origin/main /tmp/targets

set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 BASE OUT" >&2
  exit 2
fi
base="$1"
out="$2"

cd "${BUILD_WORKSPACE_DIRECTORY:-$(git rev-parse --show-toplevel)}"

bazel="${BAZEL:-bazel}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

everything() {
  printf 'affected_targets: the whole suite: %s\n' "$1"
  echo '//...' > "$out"
  exit 0
}

query() {
  "$bazel" query --noshow_progress --query_file="$1" < /dev/null
}

git diff --name-status --no-renames "$base" HEAD > "$work/status"

# "//pkg:file" and "//:file" in MODULE.bazel, as paths.
sed -n 's|.*"//\([^":]*\):\([^"]*\)".*|\1/\2|p' MODULE.bazel \
  | sed 's|^/||' | LC_ALL=C sort -u > "$work/module_files"

: > "$work/changed"
: > "$work/optional"
while IFS=$'\t' read -r status path; do
  case "$path" in
    e2e/* | examples/* | site/*)
      echo "$path" >> "$work/changed"
      echo "$path" >> "$work/optional"
      continue ;;
  esac
  case "$status" in
    D) everything "$path was deleted" ;;
  esac
  if grep -qxF "$path" "$work/module_files"; then
    everything "MODULE.bazel names $path"
  fi
  case "$path" in
    .github/* | tools/* | platforms/* | ts/toolchain/*)
      everything "$path is CI or toolchain configuration" ;;
    *.bazel | BUILD | */BUILD | *.bzl | WORKSPACE* | .bazelversion | .bazelignore | .bazelrc | */.bazelrc)
      everything "$path changes the build graph" ;;
    *pnpm-lock.yaml | *pnpm-workspace.yaml | *package.json | *.npmrc | *Cargo.lock | *Cargo.toml | *go.mod | *go.sum | go.work | go.work.sum | *.patch)
      everything "$path is a repository rule input" ;;
    *.md | mkdocs.yml | LICENSE | .gitignore | .vscode/* | .githooks/*)
      echo "$path" >> "$work/optional" ;;
  esac
  echo "$path" >> "$work/changed"
done < "$work/status"

if [ ! -s "$work/changed" ]; then
  echo "affected_targets: no file changed"
  : > "$out"
  exit 0
fi

# Which changed files are targets. --keep_going drops a path no package owns;
# the comparison below is what notices.
{
  printf 'set('
  { grep -vE '^(e2e|examples|site)/' "$work/changed" || true; } | sed 's/.*/"&"/' | tr '\n' ' '
  printf ')\n'
} > "$work/files.query"
if grep -q '"' "$work/files.query"; then
  "$bazel" query --noshow_progress --keep_going --query_file="$work/files.query" \
    < /dev/null 2> /dev/null > "$work/labels" || true
else
  : > "$work/labels"
fi
sed -e 's|^//:||' -e 's|^//\([^:]*\):|\1/|' "$work/labels" | LC_ALL=C sort -u > "$work/owned"

LC_ALL=C sort -u "$work/changed" > "$work/changed_sorted"
LC_ALL=C sort -u "$work/optional" > "$work/optional_sorted"
LC_ALL=C comm -23 "$work/changed_sorted" "$work/owned" \
  | LC_ALL=C comm -23 - "$work/optional_sorted" > "$work/unowned"
if [ -s "$work/unowned" ]; then
  everything "no target owns $(head -n 1 "$work/unowned")"
fi

if [ ! -s "$work/labels" ]; then
  echo "affected_targets: no target owns a changed file"
  : > "$out"
  exit 0
fi

echo "filter('^//', deps(kind('toolchain rule', //...)))" > "$work/toolchains.query"
query "$work/toolchains.query" | LC_ALL=C sort -u > "$work/toolchain_closure"
LC_ALL=C sort -u "$work/labels" > "$work/labels_sorted"
LC_ALL=C comm -12 "$work/labels_sorted" "$work/toolchain_closure" > "$work/in_toolchain"
if [ -s "$work/in_toolchain" ]; then
  everything "a registered toolchain is built from $(head -n 1 "$work/in_toolchain")"
fi

# `//...` as a build pattern skips `manual` targets; as a query universe it
# does not.
{
  printf 'kind(rule, rdeps(//..., set('
  tr '\n' ' ' < "$work/labels_sorted"
  printf "))) except attr(\"tags\", \"[\\\\[ ]manual[,\\\\]]\", //...)\n"
} > "$work/rdeps.query"
query "$work/rdeps.query" > "$out"

printf 'affected_targets: %s changed files reach %s targets\n' \
  "$(wc -l < "$work/changed_sorted" | tr -d ' ')" "$(wc -l < "$out" | tr -d ' ')"
