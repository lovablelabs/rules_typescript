#!/usr/bin/env bash
# Usage: tools/bench_declarations.sh [PACKAGES] [FILES_PER_PACKAGE] [ITERATIONS]
set -euo pipefail

PKGS="${1:-10}"
PER_PKG="${2:-30}"
ITERS="${3:-3}"
RULES_TS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BAZEL="${BAZEL:-$(command -v bazelisk || command -v bazel)}"
[[ -n "$BAZEL" ]] || { echo "no bazel/bazelisk on PATH" >&2; exit 1; }

# The generated workspace plus a Bazel output base runs to several GB, which a
# small /tmp tmpfs cannot hold. Override with BENCH_TMPDIR.
BENCH_TMPDIR="${BENCH_TMPDIR:-${TMPDIR:-$HOME/.cache}}"
mkdir -p "$BENCH_TMPDIR"
WS="$(mktemp -d -p "$BENCH_TMPDIR" -t rules_ts_bench.XXXXXX)"
OB="$(mktemp -d -p "$BENCH_TMPDIR" -t rules_ts_bench_ob.XXXXXX)"
cleanup() { chmod -R u+w "$OB" 2>/dev/null || true; rm -rf "$WS" "$OB"; }
trap cleanup EXIT

cat > "$WS/MODULE.bazel" <<EOF
module(name = "bench", version = "0.0.0")
bazel_dep(name = "rules_typescript", version = "0.0.0")
local_path_override(module_name = "rules_typescript", path = "$RULES_TS_ROOT")
register_toolchains("@rules_typescript//ts/toolchain:all")
EOF
printf 'build --incompatible_strict_action_env\nbuild --output_groups=+_validation\n' > "$WS/.bazelrc"
cp "$RULES_TS_ROOT/.bazelversion" "$WS/.bazelversion" 2>/dev/null || true
echo 'exports_files(["MODULE.bazel"])' > "$WS/BUILD.bazel"

# Every export is annotated, so the "oxc" arm is legal. Package N imports from
# package N-1 to build one deep chain rather than a wide fan.
gen_tree() {
  local root="$1"
  for ((p=0; p<PKGS; p++)); do
    local dir="$WS/$root/pkg$p"; mkdir -p "$dir"
    local deps=""
    [[ $p -gt 0 ]] && deps="        \"//$root/pkg$((p-1)):pkg$((p-1))\","
    for ((f=0; f<PER_PKG; f++)); do
      {
        if [[ $p -gt 0 ]]; then
          echo "import { value0 as prev } from \"../pkg$((p-1))/mod0\";"
        fi
        echo "export interface Shape$f { id: string; n: number; tags: readonly string[]; }"
        echo "export const value$f: Shape$f = { id: \"m$f\", n: $f, tags: [\"a\", \"b\"] };"
        if [[ $p -gt 0 ]]; then
          echo "export function combine$f(x: Shape$f): number { return x.n + prev.n; }"
        else
          echo "export function combine$f(x: Shape$f): number { return x.n; }"
        fi
        echo "export const PATTERN$f: RegExp = /^m[0-9]+$/;"
        echo "// bench-salt 0"
      } > "$dir/mod$f.ts"
    done
    local srcs=""
    for ((f=0; f<PER_PKG; f++)); do srcs+="        \"mod$f.ts\","$'\n'; done
    cat > "$dir/BUILD.bazel" <<EOF
load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "pkg$p",
    srcs = [
$srcs    ],
    deps = [
$deps
    ],
    visibility = ["//visibility:public"],
)
EOF
  done
}

gen_tree tsgo_tree
gen_tree oxc_tree

cd "$WS"
bz() { env -u TEST_TMPDIR "$BAZEL" --output_base="$OB" "$@"; }

echo "corpus: $PKGS packages x $PER_PKG files = $((PKGS*PER_PKG)) files per arm, linear dep chain"
echo "warming toolchains (oxc is built from Rust source on first use)..."
bz build //tsgo_tree/... >/dev/null 2>&1 || { echo "warmup build FAILED" >&2; bz build //tsgo_tree/... 2>&1 | tail -40 >&2; exit 1; }
bz build //oxc_tree/... --@rules_typescript//ts:declarations=oxc >/dev/null 2>&1 || { echo "warmup build FAILED" >&2; bz build //oxc_tree/... --@rules_typescript//ts:declarations=oxc 2>&1 | tail -40 >&2; exit 1; }

SALT=0
measure() { # arm mode -> "wall critical"
  local tree="$1"
  local mode="$2"
  local log="$OB/bench_$tree.log"
  # Rewrite a salt comment in every source: Bazel invalidates on content
  # digests, so touching mtimes rebuilds nothing.
  find "$WS/$tree" -name '*.ts' -print0 |
    xargs -0 sed -i "s|^// bench-salt .*|// bench-salt $SALT|"
  local s e
  s=$(date +%s.%N)
  bz build "//$tree/..." "--@rules_typescript//ts:declarations=$mode" >"$log" 2>&1
  e=$(date +%s.%N)
  local crit
  crit=$(grep -oE "Critical Path: [0-9.]+" "$log" | tail -1 | grep -oE "[0-9.]+" || echo "0")
  printf '%.1f %s' "$(echo "$e - $s" | bc)" "$crit"
}

printf '\n%-5s %-8s %-20s %-20s\n' "iter" "load" "tsgo (wall/crit)" "oxc+check (wall/crit)"
for ((i=1; i<=ITERS; i++)); do
  la=$(cut -d' ' -f1 /proc/loadavg)
  SALT=$((SALT + 1))
  read -r tw tc <<<"$(measure tsgo_tree tsgo)"
  read -r ow oc <<<"$(measure oxc_tree oxc)"
  printf '%-5s %-8s %-20s %-20s\n' "$i" "$la" "${tw}s / ${tc}s" "${ow}s / ${oc}s"
done
echo
echo "wall = end-to-end rebuild of that arm after touching every source"
echo "crit = Bazel's reported critical path"
echo
echo "tsgo         : TsEmit (js) + TsgoCheck (diagnostics) + TsgoDeclare (d.ts)"
echo "oxc+check    : TsEmit (js + d.ts) + TsgoCheck (diagnostics)"
