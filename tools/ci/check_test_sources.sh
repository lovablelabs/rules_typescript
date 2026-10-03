#!/usr/bin/env bash
# check_test_sources.sh -- assert that every test source file in this workspace
# is named in the srcs of some test target, and that the target actually runs.
#
# `bazel build //...`, `bazel test //...` and a byte-identical Gazelle rerun are
# all satisfied by a Gazelle run that DELETES a test target, which is how seven
# hand-written go_test targets once went missing. This check is not: the set of
# test files on disk is not something Gazelle writes, so it cannot be brought
# back into agreement by damaging the BUILD files.
#
# Tagging a test `manual` is the same regression wearing a disguise -- the target
# still exists, still claims its srcs, and `bazel test //...` still passes,
# because `//...` skips it. So the file's claim is checked twice: once against
# every test target, and once against only the targets `bazel test //...` runs.
# A file that has the first claim but not the second is manual-only, and must be
# named in MANUAL_ONLY below with a reason. The list is exact in both directions,
# so tagging a test `manual` fails here until someone writes down why, and
# untagging it fails until the entry is removed.
#
# Read-only -- a loading-phase query and `git ls-files`. It never runs Gazelle
# and never writes to the source tree.
#
# One limit it does not cover: `git ls-files` cannot see an unstaged new file, so
# a local run reports green on a test that has no target yet.
#
#   tools/ci/check_test_sources.sh
#   BAZEL=bazelisk tools/ci/check_test_sources.sh

set -euo pipefail

cd "${BUILD_WORKSPACE_DIRECTORY:-$(git rev-parse --show-toplevel)}"

# Files whose only test target is tagged `manual`. Each needs a reason: a
# manual-only test is one nothing executes, so the entry is the argument for why
# that is the intended outcome rather than an accident.
MANUAL_ONLY=$(
  cat <<'ALLOWLIST'
# Analysis-only fixtures. Each ts_test target is asserted to FAIL at analysis:
# a test_srcs root that is not in srcs, and test_srcs selecting no program input.
tests/node_test/analysis/undeclared.test.ts
tests/node_test/analysis/empty.test.ts
# Analysis-only fixture: joined_scope_inputs_test inspects the program's inputs;
# running the test adds nothing it asserts.
tests/compile_layout/joined_scope/scope.test.ts
# Analysis-only fixture, asserted to FAIL at analysis: the wrangler config it
# stages through `wrangler_config` is in its `data` too.
tests/workers_nested/test/data_shadow.test.ts
# Denied by the sandbox by design: the read outside the runfiles that
# `bazel run //tests/vitest/reads_report:reads_report_test -- --reads` names;
# reads_declared_test makes it with data set.
tests/vitest/reads_report/reads_report.test.ts
ALLOWLIST
)

bazel="${BAZEL:-bazel}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf '%s\n' "$MANUAL_ONLY" \
  | sed -e 's/#.*//' -e 's/[[:space:]]*$//' \
  | awk 'NF' | LC_ALL=C sort -u > "$work/manual_allowed"

# A directory holding its own MODULE.bazel is a separate workspace that `//...`
# does not descend into, so its test files are not this workspace's to claim;
# .bazelignore roots are invisible to `//...` for the same practical reason.
{
  git ls-files '*/MODULE.bazel' | sed 's|/[^/]*$||'
  if [ -f .bazelignore ]; then
    sed -e 's/#.*//' -e 's:/*[[:space:]]*$::' .bazelignore
  fi
} | awk 'NF' | LC_ALL=C sort -u > "$work/out_of_scope"

git ls-files \
  '*_test.go' '*.test.ts' '*.test.tsx' '*.test.mts' \
  '*.spec.ts' '*.spec.tsx' '*.spec.mts' \
  | awk -v roots="$(cat "$work/out_of_scope")" '
      BEGIN { n = split(roots, root, "\n") }
      {
        for (i = 1; i <= n; i++)
          if (index($0, root[i] "/") == 1) next
        print
      }' \
  | while IFS= read -r file; do
      if [ -f "$file" ]; then printf '%s\n' "$file"; fi
    done \
  | LC_ALL=C sort -u > "$work/on_disk"

all_tests='tests(//...)'

# `attr` matches its regex against the stringified attribute value, so the tags
# list arrives as `[manual, other]` -- hence the delimiter classes rather than an
# anchored `^manual$`, which never matches.
runnable_tests='(let t = tests(//...) in $t except attr("tags", "[\[ ]manual[,\]]", $t))'

# `bazel query` prints partial results and exits 7 on a package-load error, so
# the status is checked before anything reads the output.
query_srcs() {
  local scope="$1" out="$2"
  local q='filter("(_test\.go|\.(test|spec)\.(ts|tsx|mts))$", labels(srcs, '"$scope"'))'
  if ! "$bazel" query --noshow_progress "$q" > "$out.raw"; then
    echo "check_test_sources: bazel query failed; see the error above." >&2
    exit 1
  fi
  sed -n 's|^//||p' "$out.raw" \
    | sed -e 's|:|/|' -e 's|^/||' \
    | LC_ALL=C sort -u > "$out"
}

query_srcs "$all_tests" "$work/claimed_any"
query_srcs "$runnable_tests" "$work/claimed_running"

status=0

LC_ALL=C comm -23 "$work/on_disk" "$work/claimed_any" > "$work/orphans"
if [ -s "$work/orphans" ]; then
  cat >&2 <<MESSAGE
check_test_sources: these test source files are not in the srcs of any test
target, so nothing runs them:

$(sed 's/^/  /' "$work/orphans")

Either add them to a test target, or delete them. A file losing its target is
usually a Gazelle run that dropped a hand-written rule -- check the BUILD file
in its package against git history before regenerating.
MESSAGE
  status=1
fi

LC_ALL=C comm -12 "$work/on_disk" "$work/claimed_any" > "$work/claimed_on_disk"
LC_ALL=C comm -23 "$work/claimed_on_disk" "$work/claimed_running" > "$work/manual_only"

if ! LC_ALL=C comm -23 "$work/manual_only" "$work/manual_allowed" > "$work/manual_unlisted"; then
  echo "check_test_sources: comm failed." >&2
  exit 1
fi

if [ -s "$work/manual_unlisted" ]; then
  cat >&2 <<MESSAGE
check_test_sources: these test source files have a test target, but every target
claiming them is tagged \`manual\`, so \`bazel test //...\` never runs them:

$(sed 's/^/  /' "$work/manual_unlisted")

Drop the \`manual\` tag, or -- if the target is deliberately analysis-only -- add
the file to MANUAL_ONLY in this script with the reason it cannot run.
MESSAGE
  status=1
fi

LC_ALL=C comm -13 "$work/manual_only" "$work/manual_allowed" > "$work/manual_stale"
if [ -s "$work/manual_stale" ]; then
  cat >&2 <<MESSAGE
check_test_sources: these MANUAL_ONLY entries are stale -- each file either no
longer exists, or is now claimed by a test target that runs:

$(sed 's/^/  /' "$work/manual_stale")

Remove them from MANUAL_ONLY in this script.
MESSAGE
  status=1
fi

if [ "$status" -ne 0 ]; then
  exit "$status"
fi

printf 'check_test_sources: %s test source files, all claimed by a test target (%s manual-only, allowlisted).\n' \
  "$(wc -l < "$work/on_disk" | tr -d ' ')" \
  "$(wc -l < "$work/manual_only" | tr -d ' ')"
