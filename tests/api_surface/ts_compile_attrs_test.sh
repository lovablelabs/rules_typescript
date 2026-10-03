#!/usr/bin/env bash
# Starlark rules cannot enumerate their own attributes.

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

RUNFILES="${TEST_SRCDIR:-}"
[[ -n "${RUNFILES}" ]] || fail "TEST_SRCDIR is unset; run this through bazel test"
if [[ -d "${RUNFILES}/_main" ]]; then
  RUNFILES="${RUNFILES}/_main"
fi

RULE="${RUNFILES}/ts/private/rules/ts_compile.bzl"
[[ -f "${RULE}" ]] || fail "missing runfile: ${RULE}"

want="${TEST_TMPDIR}/want"
got="${TEST_TMPDIR}/got"

printf '%s\n' data deps emit node_modules package_scopes source_node_modules srcs tsconfig type_inputs > "${want}"

# The dict runs from `TS_COMPILE_ATTRS = {` to the closing brace at column 0; a
# public attribute is a 4-space-indented quoted key at that depth.
sed -n '/^TS_COMPILE_ATTRS = {$/,/^}$/p' "${RULE}" \
  | sed -n 's/^    "\([a-z][a-z_]*\)": attr\..*/\1/p' \
  | LC_ALL=C sort > "${got}"

if ! LC_ALL=C diff -u "${want}" "${got}" > "${TEST_TMPDIR}/diff"; then
  echo "ts_compile's public attributes differ from the pinned set (-want +got):" >&2
  cat "${TEST_TMPDIR}/diff" >&2
  exit 1
fi
echo "ts_compile has exactly $(wc -l < "${want}" | tr -d ' ') public attributes"
