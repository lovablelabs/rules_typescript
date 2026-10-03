#!/usr/bin/env bash
# The runner seam as one set: what TsTestRunnerInfo.launch's doc names, both
# runners return and the core reads. A doc can name what nothing produces.

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

RUNFILES="${TEST_SRCDIR:-}"
[[ -n "${RUNFILES}" ]] \
  || fail "TEST_SRCDIR is unset; run this through bazel test"
if [[ -d "${RUNFILES}/_main" ]]; then
  RUNFILES="${RUNFILES}/_main"
fi

PROVIDERS="${RUNFILES}/ts/private/providers.bzl"
RUNNERS="${RUNFILES}/ts/private/rules/runners.bzl"
CORE="${RUNFILES}/ts/private/rules/ts_test.bzl"
for f in "${PROVIDERS}" "${RUNNERS}" "${CORE}"; do
  [[ -f "${f}" ]] || fail "missing runfile: ${f}"
done

want="${TEST_TMPDIR}/want"
got="${TEST_TMPDIR}/got"

check() {
  if ! LC_ALL=C diff -u "${want}" "${got}" > "${TEST_TMPDIR}/diff"; then
    echo "$1 differ from the pinned set (-want +got):" >&2
    cat "${TEST_TMPDIR}/diff" >&2
    exit 1
  fi
}

doc="$(sed -n '/^TsTestRunnerInfo = provider($/,/^)$/p' "${PROVIDERS}" \
  | awk '/^    },$/ { on = 0 } /^        "/ { on = /^        "launch": / } on' \
  | sed -e 's/^ *"launch": "//' -e 's/^ *"//' -e 's/" +$//' -e 's/",$//' \
  | tr -d '\n')"
[[ -n "${doc}" ]] || fail "no \"launch\" field in TsTestRunnerInfo"

printf '%s\n' asset_files canonical_links chain entry_extensions entry_points \
  es_twins inline_members package_sources placed runner runtime_data_sets \
  runtime_files runtime_inputs runtime_sources test_files_list transitive_js \
  > "${want}"
printf '%s\n' "${doc}" \
  | sed -n 's/.*builds from the compile (\([^)]*\)).*/\1/p' \
  | tr ', ' '\n\n' | awk 'NF' | LC_ALL=C sort > "${got}"
check "the test members launch's doc names"
grep -q '^    launched = runner.launch(ctx, test)$' "${CORE}" \
  || fail "the core does not launch the runner with its test struct"
sed -n '/^    test = struct($/,/^    )$/p' "${CORE}" \
  | sed -n 's/^        \([a-z_]*\) = .*/\1/p' | LC_ALL=C sort > "${got}"
check "the test members the core builds"

printf '%s\n' env files mode output_groups replacements section symlinks \
  transitive_files > "${want}"
printf '%s\n' "${doc}" | sed 's/.*[Tt]he result carries//' \
  | grep -o '`[a-z_]*`' | tr -d '`' | LC_ALL=C sort -u > "${got}"
check "the result members launch's doc names"
{
  grep -o 'launched\.[a-z_]*' "${CORE}" | sed 's/launched\.//'
  grep -o 'getattr(launched, "[a-z_]*"' "${CORE}" | sed 's/.*"\([a-z_]*\)"/\1/'
} | LC_ALL=C sort -u > "${got}"
check "the result members the core reads"

awk '/^def / { name = "" }
     /^def _[a-z_]*_launch\(/ { name = $2; sub(/\(.*/, "", name) }
     name != "" && /^    return struct\($/ { open = 1; next }
     open && /^    \)$/ { open = 0 }
     open && match($0, /^        [a-z_]+ = /) {
       print name, substr($0, 9, RLENGTH - 11)
     }' "${RUNNERS}" > "${TEST_TMPDIR}/returned"
launches="$(cut -d' ' -f1 "${TEST_TMPDIR}/returned" | LC_ALL=C sort -u)"
[[ "${launches}" == $'_node_test_launch\n_vitest_launch' ]] \
  || fail "the launch functions returning a struct: ${launches//$'\n'/ }"
optional="replacements"
for launch in ${launches}; do
  grep -vx "${optional}" "${want}" > "${TEST_TMPDIR}/required"
  awk -v n="${launch}" -v o="${optional}" '$1 == n && $2 != o { print $2 }' \
    "${TEST_TMPDIR}/returned" | LC_ALL=C sort > "${got}"
  LC_ALL=C diff -u "${TEST_TMPDIR}/required" "${got}" > "${TEST_TMPDIR}/diff" \
    || { cat "${TEST_TMPDIR}/diff" >&2; fail "the result members ${launch} returns differ from the required set"; }
done
cut -d' ' -f2 "${TEST_TMPDIR}/returned" | LC_ALL=C sort -u > "${got}"
check "the result members the runners return together"
echo "TsTestRunnerInfo.launch: 16 test members and 8 result members," \
  "named by the doc, returned by the runners, read by the core"
