#!/usr/bin/env bash

# --- begin runfiles.bash initialization v3 ---
# Copy-pasted from the Bazel Bash runfiles library v3.
set -uo pipefail; set +e; f=bazel_tools/tools/bash/runfiles/runfiles.bash
# shellcheck disable=SC1090
source "${RUNFILES_DIR:-/dev/null}/$f" 2>/dev/null || \
  source "$(grep -sm1 "^$f " "${RUNFILES_MANIFEST_FILE:-/dev/null}" | cut -f2- -d' ')" 2>/dev/null || \
  source "$0.runfiles/$f" 2>/dev/null || \
  source "$(grep -sm1 "^$f " "$0.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null || \
  source "$(grep -sm1 "^$f " "$0.exe.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null || \
  { echo>&2 "ERROR: cannot find $f"; exit 1; }; f=; set -e
# --- end runfiles.bash initialization v3 ---

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# rlocation answers an absent runfile with a non-zero exit and no output, which
# would otherwise become an empty path handed to node.
runfile() {
  local resolved
  resolved="$(rlocation "${TEST_WORKSPACE:-_main}/$1")" || resolved=""
  [[ -n "${resolved}" && -e "${resolved}" ]] || fail "missing runfile: $1"
  printf '%s\n' "${resolved}"
}

NODE="$(runfile ts/toolchain/node_resolved/node)"
PLUGIN_TEST_MJS="$(runfile tests/lsp/tsserver_plugin_test.mjs)"
NODE_MODULES="$(runfile tests/lsp/node_modules)"
REFRESH="$(runfile refresh_tsconfig)"
GENERATED_ENTRYPOINT_CONFIG="$(runfile tests/lsp/generated_entrypoint_config.json)"
[[ -d "${NODE_MODULES}" ]] || fail "not a node_modules tree: ${NODE_MODULES}"

echo "INFO: node $("${NODE}" --version)"

TSSERVER_JS="${NODE_MODULES}/typescript/lib/tsserver.js"
# A host install would satisfy this silently, which is exactly the
# non-hermeticity to keep out of a test about editor behaviour.
[[ -f "${TSSERVER_JS}" ]] || \
  fail "no tsserver at ${TSSERVER_JS} -- is @npm//:typescript still a dep of" \
    "//tests/lsp:node_modules?"

# ── Stage what refresh_tsconfig installs, into a scratch workspace ────────────
# The copier reads its manifest from the runfiles tree this test already has, so
# pointing BUILD_WORKSPACE_DIRECTORY at TEST_TMPDIR is the whole of the setup.
WORKSPACE_ROOT="${TEST_TMPDIR:?TEST_TMPDIR is unset}/workspace"
mkdir -p "${WORKSPACE_ROOT}"

BUILD_WORKSPACE_DIRECTORY="${WORKSPACE_ROOT}" \
    COPY_TO_WORKSPACE_MANIFEST="${TEST_WORKSPACE:-_main}/refresh_tsconfig.manifest.json" \
    "${REFRESH}" > "${TEST_TMPDIR}/copied.txt"
echo "INFO: staged $(wc -l < "${TEST_TMPDIR}/copied.txt") files into ${WORKSPACE_ROOT}"

# The marker the plugin walks up to find, and which the manifest does not carry.
touch "${WORKSPACE_ROOT}/MODULE.bazel"

exec "${NODE}" "${PLUGIN_TEST_MJS}" "${TSSERVER_JS}" "${WORKSPACE_ROOT}" "${REFRESH}" "${GENERATED_ENTRYPOINT_CONFIG}"
