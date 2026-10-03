"""Unit tests for the Starlark helpers behind npm repository generation.

These functions decide repository names, label names and dependency edges, and
until now every one of them was reachable only through a full npm build: a
change in behaviour showed up as a mysterious analysis error in an unrelated
test, or not at all.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//npm:lazy.bzl", "platforms_of_package")
load(
    "//npm/private:npm_translate_lock.bzl",
    "dep_snapshot_id",
    "npm_tarball_url",
    "package_dir_name",
    "package_name_to_label",
    "peer_suffix_dir_name",
    "pkg_matches_platform",
    "semver_gt",
    "semver_parts",
    "snapshot_dir_name",
    "snapshot_parts",
    "versioned_label_name",
)

# ─── Label and directory naming ───────────────────────────────────────────────
# The gazelle resolver builds @npm labels from package names independently (see
# npmPackageToLabelName in gazelle/resolve.go). These cases are the shared
# contract: if this side changes, generated BUILD files stop resolving.

def _package_name_to_label_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "vitest", package_name_to_label("vitest"))
    asserts.equals(env, "react-dom", package_name_to_label("react-dom"))
    asserts.equals(env, "types_react", package_name_to_label("@types/react"))
    asserts.equals(env, "tanstack_router", package_name_to_label("@tanstack/router"))
    asserts.equals(env, "lodash.debounce", package_name_to_label("lodash.debounce"))

    return unittest.end(env)

package_name_to_label_test = unittest.make(_package_name_to_label_test)

def _package_dir_name_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "types_react__19_0_0", package_dir_name("@types/react", "19.0.0"))
    asserts.equals(env, "react__19_1_0", package_dir_name("react", "19.1.0"))

    # Every character that is illegal in a directory name has to go: a
    # pre-release and a build-metadata version must not leak "-" or "+".
    asserts.equals(env, "next__15_0_0_rc_1", package_dir_name("next", "15.0.0-rc.1"))
    asserts.equals(env, "pkg__1_0_0_build_5", package_dir_name("pkg", "1.0.0+build.5"))

    # Two versions of one package must not collide on one directory.
    asserts.true(
        env,
        package_dir_name("react", "18.3.1") != package_dir_name("react", "19.1.0"),
        "two versions of react collided on one directory name",
    )

    return unittest.end(env)

package_dir_name_test = unittest.make(_package_dir_name_test)

def _versioned_label_name_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "react_19_1_0", versioned_label_name("react", "19.1.0"))
    asserts.equals(
        env,
        "vitest_pretty-format_3_0_9",
        versioned_label_name("vitest_pretty-format", "3.0.9"),
    )
    asserts.equals(env, "next_15_0_0_rc_1", versioned_label_name("next", "15.0.0-rc.1"))
    asserts.equals(env, "pkg_1_0_0_build_5", versioned_label_name("pkg", "1.0.0+build.5"))

    return unittest.end(env)

versioned_label_name_test = unittest.make(_versioned_label_name_test)

def _npm_tarball_url_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(
        env,
        "https://registry.npmjs.org/zod/-/zod-3.24.1.tgz",
        npm_tarball_url("zod", "3.24.1", {}),
    )

    # A scoped package's tarball lives under the scope but is named without it.
    asserts.equals(
        env,
        "https://registry.npmjs.org/@types/react/-/react-19.0.0.tgz",
        npm_tarball_url("@types/react", "19.0.0", {}),
    )

    # An explicit tarball in the lockfile's resolution wins: this is how a
    # git/https dependency keeps its own URL instead of a registry guess.
    asserts.equals(
        env,
        "https://example.test/custom.tgz",
        npm_tarball_url("zod", "3.24.1", {"tarball": "https://example.test/custom.tgz"}),
    )

    return unittest.end(env)

npm_tarball_url_test = unittest.make(_npm_tarball_url_test)

# ─── Snapshot identity ────────────────────────────────────────────────────────

def _snapshot_parts_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, ("foo@1.0.0", ""), snapshot_parts("foo@1.0.0"))
    asserts.equals(env, ("foo@1.0.0", "(react@18.3.1)"), snapshot_parts("foo@1.0.0(react@18.3.1)"))

    # Quoted, slash-prefixed and nested-peer forms all appear in real lockfiles.
    asserts.equals(
        env,
        ("@babel/core@7.26.0", "(supports-color@8.1.1)"),
        snapshot_parts("'@babel/core@7.26.0(supports-color@8.1.1)'"),
    )
    asserts.equals(
        env,
        ("vitest@3.0.9", "(@types/node@22.10.2)(jsdom@25.0.1)"),
        snapshot_parts("/vitest@3.0.9(@types/node@22.10.2)(jsdom@25.0.1)"),
    )

    return unittest.end(env)

snapshot_parts_test = unittest.make(_snapshot_parts_test)

def _dep_snapshot_id_test(ctx):
    env = unittest.begin(ctx)

    # The value is relative to the dependency name, so the key is reassembled.
    asserts.equals(env, "vitest@3.0.9(vite@6.4.1)", dep_snapshot_id("vitest", "3.0.9(vite@6.4.1)"))
    asserts.equals(env, "zod@3.24.1", dep_snapshot_id("zod", "'3.24.1'"))

    # An npm alias value already carries the aliased package's own name.
    asserts.equals(env, "lodash@4.17.21", dep_snapshot_id("underscore", "lodash@4.17.21"))

    # A `file:` tarball is keyed like a package; workspace and directory deps are not.
    asserts.equals(env, "shared@file:vendor/shared.tgz", dep_snapshot_id("shared", "file:vendor/shared.tgz"))
    asserts.equals(env, "", dep_snapshot_id("shared", "link:../shared"))
    asserts.equals(env, "", dep_snapshot_id("shared", "file:../shared"))
    asserts.equals(env, "", dep_snapshot_id("shared", ""))

    return unittest.end(env)

dep_snapshot_id_test = unittest.make(_dep_snapshot_id_test)

def _peer_suffix_dir_name_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "", peer_suffix_dir_name(""))

    one = peer_suffix_dir_name("(react@18.3.1)")
    two = peer_suffix_dir_name("(react@19.1.0)")

    # Two peer sets of the same package must never land in one repository.
    asserts.true(env, one != two, "distinct peer sets produced the same directory component")

    # Stable across calls: the repository name goes into MODULE.bazel.lock.
    asserts.equals(env, one, peer_suffix_dir_name("(react@18.3.1)"))

    # Only directory-safe characters survive, and no doubled or edge separators.
    safe = "abcdefghijklmnopqrstuvwxyz0123456789_-"
    for ch in one.elems():
        asserts.true(env, ch in safe, "unsafe character {} in {}".format(ch, one))
    asserts.true(env, one.find("__") == -1, "doubled underscore in " + one)
    asserts.true(env, not one.startswith("_") and not one.endswith("_"), "edge underscore in " + one)

    # A very long suffix is truncated for readability but stays unique, which is
    # the whole reason a digest is appended.
    long_a = "(react@18.3.1)(@types/node@22.10.2)(typescript@5.7.2)(vite@6.4.1)(jsdom@25.0.1)"
    long_b = long_a + "(zod@3.24.1)"
    dir_a = peer_suffix_dir_name(long_a)
    dir_b = peer_suffix_dir_name(long_b)
    asserts.true(env, len(dir_a) <= 49, "truncation cap exceeded: {}".format(dir_a))
    asserts.true(env, dir_a != dir_b, "two long peer sets collided: {}".format(dir_a))

    return unittest.end(env)

peer_suffix_dir_name_test = unittest.make(_peer_suffix_dir_name_test)

def _snapshot_dir_name_test(ctx):
    env = unittest.begin(ctx)

    peer_free = {"name": "zod", "version": "3.24.1", "peer_suffix": ""}
    asserts.equals(env, "zod__3_24_1", snapshot_dir_name(peer_free))

    with_peers = {"name": "vitest", "version": "3.0.9", "peer_suffix": "(vite@6.4.1)"}
    got = snapshot_dir_name(with_peers)
    asserts.true(
        env,
        got.startswith("vitest__3_0_9__"),
        "peer-bearing snapshot lost its base name: {}".format(got),
    )
    asserts.true(
        env,
        got != snapshot_dir_name({"name": "vitest", "version": "3.0.9", "peer_suffix": "(vite@5.0.0)"}),
        "two vitest resolutions collided on one directory",
    )

    return unittest.end(env)

snapshot_dir_name_test = unittest.make(_snapshot_dir_name_test)

# ─── Version comparison ───────────────────────────────────────────────────────

def _semver_parts_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, [1, 2, 3, 0, []], semver_parts("1.2.3"))
    asserts.equals(env, [1, 2, 0, 0, []], semver_parts("1.2"))
    asserts.equals(env, [1, 0, 0, 0, []], semver_parts("1"))

    # major.minor.patch is exactly three entries whatever trails it, so the
    # pre-release flag semver_gt reads is always the one at index 3.
    asserts.equals(env, [1, 2, 3, 0, []], semver_parts("1.2.3.4"))
    asserts.equals(env, [19, 0, 0, 1, [(1, 0, "rc"), (0, 1, "")]], semver_parts("19.0.0-rc.1"))
    asserts.equals(env, [15, 1, 0, 1, [(1, 0, "canary"), (0, 3, "")]], semver_parts("15.1.0-canary.3"))
    asserts.equals(env, [19, 0, 0, 1, [(1, 0, "rc")]], semver_parts("19.0.0-rc"))

    # Build metadata has no precedence, so it parses away entirely.
    asserts.equals(env, semver_parts("1.0.0"), semver_parts("1.0.0+build.5"))
    asserts.equals(env, semver_parts("1.0.0-rc.1"), semver_parts("1.0.0-rc.1+build.5"))

    # A '-' is only a pre-release marker in the part metadata was stripped from.
    asserts.equals(env, [1, 0, 0, 0, []], semver_parts("1.0.0+build-5"))

    return unittest.end(env)

semver_parts_test = unittest.make(_semver_parts_test)

def _semver_gt_test(ctx):
    env = unittest.begin(ctx)

    def gt(a, b):
        return semver_gt(semver_parts(a), semver_parts(b))

    asserts.true(env, gt("19.1.0", "18.3.1"), "19.1.0 > 18.3.1")
    asserts.true(env, gt("1.2.10", "1.2.9"), "1.2.10 > 1.2.9")
    asserts.true(env, gt("1.3.0", "1.2.99"), "1.3.0 > 1.2.99")
    asserts.false(env, gt("18.3.1", "19.1.0"), "18.3.1 is not > 19.1.0")

    # Strictly greater: equal versions are not greater than each other.
    asserts.false(env, gt("1.2.3", "1.2.3"), "1.2.3 is not > itself")

    # A release outranks its own pre-releases, in both directions, whatever the
    # pre-release tail is -- a numeric tail is not a version component.
    asserts.true(env, gt("19.0.0", "19.0.0-rc.1"), "19.0.0 > 19.0.0-rc.1")
    asserts.false(env, gt("19.0.0-rc.1", "19.0.0"), "19.0.0-rc.1 is not > 19.0.0")
    asserts.true(env, gt("19.0.0", "19.0.0-rc.0"), "19.0.0 > 19.0.0-rc.0")
    asserts.false(env, gt("19.0.0-rc.0", "19.0.0"), "19.0.0-rc.0 is not > 19.0.0")
    asserts.true(env, gt("19.0.0", "19.0.0-rc"), "19.0.0 > 19.0.0-rc")

    # Pre-releases of one version order against each other, so which of two
    # release candidates wins does not fall out of iteration order.
    asserts.true(env, gt("1.0.0-rc.2", "1.0.0-rc.1"), "1.0.0-rc.2 > 1.0.0-rc.1")
    asserts.false(env, gt("1.0.0-rc.1", "1.0.0-rc.2"), "1.0.0-rc.1 is not > 1.0.0-rc.2")
    asserts.true(env, gt("1.0.0-rc.10", "1.0.0-rc.9"), "numeric identifiers compare numerically")
    asserts.false(env, gt("1.0.0-rc.1", "1.0.0-rc.1"), "1.0.0-rc.1 is not > itself")

    # The precedence chain from the semver spec, each step in both directions.
    chain = [
        "1.0.0-alpha",
        "1.0.0-alpha.1",
        "1.0.0-alpha.beta",
        "1.0.0-beta",
        "1.0.0-beta.2",
        "1.0.0-beta.11",
        "1.0.0-rc.1",
        "1.0.0",
    ]
    for i in range(1, len(chain)):
        asserts.true(env, gt(chain[i], chain[i - 1]), "{} > {}".format(chain[i], chain[i - 1]))
        asserts.false(env, gt(chain[i - 1], chain[i]), "{} is not > {}".format(chain[i - 1], chain[i]))

    # Build metadata is ignored for precedence: neither side is greater.
    asserts.false(env, gt("1.0.0", "1.0.0+build.5"), "build metadata does not lower a release")
    asserts.false(env, gt("1.0.0+build.5", "1.0.0"), "build metadata does not raise a release")

    # A fourth component is not part of the comparison either.
    asserts.false(env, gt("1.2.3.4", "1.2.3"), "1.2.3.4 is not > 1.2.3")
    asserts.false(env, gt("1.2.3", "1.2.3.4"), "1.2.3 is not > 1.2.3.4")

    return unittest.end(env)

semver_gt_test = unittest.make(_semver_gt_test)

# ─── Platform constraints ─────────────────────────────────────────────────────

def _pkg_matches_platform_test(ctx):
    env = unittest.begin(ctx)

    asserts.true(env, pkg_matches_platform({}, "linux", "x64", "glibc"), "no constraint admits everything")
    asserts.true(env, pkg_matches_platform({"os": ["linux"]}, "linux", "arm64", "glibc"), "os-only match")
    asserts.false(env, pkg_matches_platform({"os": ["darwin"]}, "linux", "x64", "glibc"), "os mismatch")
    asserts.false(env, pkg_matches_platform({"cpu": ["arm64"]}, "linux", "x64", "glibc"), "cpu mismatch")

    # All present: all must match.
    both = {"os": ["linux"], "cpu": ["x64"]}
    asserts.true(env, pkg_matches_platform(both, "linux", "x64", "glibc"), "all match")
    asserts.false(env, pkg_matches_platform(both, "linux", "arm64", "glibc"), "cpu half of a pair mismatches")
    asserts.false(env, pkg_matches_platform(both, "darwin", "x64", "glibc"), "os half of a pair mismatches")

    # libc is the only axis on which a native sidecar's two linux builds differ:
    # @oxlint/linux-x64-gnu and @oxlint/linux-x64-musl agree on os and cpu.
    gnu = {"os": ["linux"], "cpu": ["x64"], "libc": ["glibc"]}
    musl = {"os": ["linux"], "cpu": ["x64"], "libc": ["musl"]}
    asserts.true(env, pkg_matches_platform(gnu, "linux", "x64", "glibc"), "glibc package on a glibc platform")
    asserts.false(env, pkg_matches_platform(musl, "linux", "x64", "glibc"), "musl package on a glibc platform")
    asserts.false(env, pkg_matches_platform({"libc": ["glibc"]}, "darwin", "arm64", ""), "darwin names no libc")

    return unittest.end(env)

pkg_matches_platform_test = unittest.make(_pkg_matches_platform_test)

def _platforms_of_package_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(
        env,
        ["linux_amd64"],
        platforms_of_package({"os": ["linux"], "cpu": ["x64"], "libc": ["glibc"]}),
        "a glibc sidecar reaches only the glibc platform",
    )
    asserts.equals(
        env,
        [],
        platforms_of_package({"os": ["linux"], "cpu": ["x64"], "libc": ["musl"]}),
        "a musl sidecar reaches no platform, so nothing declares it",
    )
    asserts.equals(
        env,
        ["linux_amd64", "linux_arm64"],
        platforms_of_package({"os": ["linux"]}),
        "a libc-less linux package reaches every linux platform",
    )
    asserts.equals(env, [], platforms_of_package({"os": ["aix"]}), "an unnameable platform reaches none")

    return unittest.end(env)

platforms_of_package_test = unittest.make(_platforms_of_package_test)

def helpers_test_suite(name):
    unittest.suite(
        name,
        package_name_to_label_test,
        package_dir_name_test,
        versioned_label_name_test,
        npm_tarball_url_test,
        snapshot_parts_test,
        dep_snapshot_id_test,
        peer_suffix_dir_name_test,
        snapshot_dir_name_test,
        semver_parts_test,
        semver_gt_test,
        pkg_matches_platform_test,
        platforms_of_package_test,
    )
