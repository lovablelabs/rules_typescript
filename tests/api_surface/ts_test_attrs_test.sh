#!/usr/bin/env bash
# Starlark rules cannot enumerate their own attributes.

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

RUNFILES="${TEST_SRCDIR:-}"
[[ -n "${RUNFILES}" ]] \
  || fail "TEST_SRCDIR is unset; run this through bazel test"
if [[ -d "${RUNFILES}/_main" ]]; then
  RUNFILES="${RUNFILES}/_main"
fi

want="${TEST_TMPDIR}/want"
got="${TEST_TMPDIR}/got"

printf '%s\n' config config_node_modules config_srcs coverage_provider data deps emit env node_modules package_scopes \
  runner source_node_modules srcs test_srcs tsconfig type_inputs workers_pool wrangler_config > "${want}"

# A dict runs from `<NAME> = {` to the closing brace at column 0; a public
# attribute is a 4-space-indented quoted key at that depth.
public_keys() {
  local file="${RUNFILES}/$1" name="$2"
  [[ -f "${file}" ]] || fail "missing runfile: ${file}"
  sed -n "/^${name} = {\$/,/^}\$/p" "${file}" \
    | sed -n 's/^    "\([a-z][a-z_]*\)": attr\..*/\1/p'
}

{
  public_keys ts/private/rules/ts_compile.bzl TS_COMPILE_ATTRS
  public_keys ts/private/rules/ts_test.bzl _TEST_ATTRS
  public_keys ts/private/actions/workers_pool.bzl WORKERS_POOL_ATTRS
} | LC_ALL=C sort -u > "${got}"

if ! LC_ALL=C diff -u "${want}" "${got}" > "${TEST_TMPDIR}/diff"; then
  echo "ts_test's public attributes differ from the pinned set" \
    "(-want +got):" >&2
  cat "${TEST_TMPDIR}/diff" >&2
  exit 1
fi
echo "ts_test has exactly $(wc -l < "${want}" | tr -d ' ') public attributes"
