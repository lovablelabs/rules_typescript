#!/usr/bin/env bash
# Source inspection also detects unwanted providers and fields.

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

RUNFILES="${TEST_SRCDIR:-}"
[[ -n "${RUNFILES}" ]] \
  || fail "TEST_SRCDIR is unset; run this through bazel test"
if [[ -d "${RUNFILES}/_main" ]]; then
  RUNFILES="${RUNFILES}/_main"
fi

PROVIDERS="${RUNFILES}/ts/private/providers.bzl"
[[ -f "${PROVIDERS}" ]] || fail "missing runfile: ${PROVIDERS}"

want="${TEST_TMPDIR}/want"
got="${TEST_TMPDIR}/got"

printf '%s\n' BundlerInfo DevServerInfo NodeModulesInfo NpmHoistInfo \
  NpmLinkInfo NpmPackageInfo TsConfigInfo TsInfo TsTestRunnerInfo > "${want}"
sed -n 's/^\([A-Za-z]*\) = provider($/\1/p' "${PROVIDERS}" \
  | LC_ALL=C sort > "${got}"
if ! LC_ALL=C diff -u "${want}" "${got}" > "${TEST_TMPDIR}/diff"; then
  echo "the providers differ from the pinned set (-want +got):" >&2
  cat "${TEST_TMPDIR}/diff" >&2
  exit 1
fi

# TsInfo runs from `TsInfo = provider(` to the closing paren at column 0; a
# field is an 8-space-indented quoted key at that depth.
printf '%s\n' data declarations js js_maps manifest npm_files npm_packages npm_view_files \
  owners runtime_source_owners runtime_sources sources transitive_data transitive_es_twins transitive_js \
  transitive_js_maps transitive_runtime_sources tsconfig > "${want}"
sed -n '/^TsInfo = provider($/,/^)$/p' "${PROVIDERS}" \
  | sed -n 's/^        "\([a-z_]*\)": .*/\1/p' \
  | LC_ALL=C sort > "${got}"
if ! LC_ALL=C diff -u "${want}" "${got}" > "${TEST_TMPDIR}/diff"; then
  echo "TsInfo's fields differ from the pinned set (-want +got):" >&2
  cat "${TEST_TMPDIR}/diff" >&2
  exit 1
fi
echo "providers.bzl and TsInfo match the pinned public surface"
