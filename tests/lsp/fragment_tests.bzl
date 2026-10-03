"""Analysis-time proof that the `ide_fragments` group reaches a private target.

Visibility governs dependency edges. An aspect propagates along the edges a
build already has and creates none, so it needs no grant where a rule's `deps`
would: //tests/lsp/fragment_fixture:leaf is `//visibility:private`, and naming it
from this package -- here, or in `ide_tsconfig(deps = [...])`, or in a filegroup
-- is a visibility error. Applying the aspect to :root reaches it anyway, which
is the whole reason the fragments exist.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//ts/private:tsconfig_aspect.bzl", "FRAGMENT_SUFFIX", "TsconfigFragmentInfo", "tsconfig_aspect")

def _fragments_reach_private_impl(ctx):
    env = analysistest.begin(ctx)
    fragments = analysistest.target_under_test(env)[TsconfigFragmentInfo].fragments.to_list()

    asserts.equals(
        env,
        [
            "//tests/lsp/fragment_fixture:leaf",
            "//tests/lsp/fragment_fixture:root",
        ],
        sorted(["//{}:{}".format(f.owner.package, f.owner.name) for f in fragments]),
        "the aspect writes a fragment for the private target as well as the public one",
    )

    # //tests/lsp:test_fragment_map reads these by path out of its runfiles.
    asserts.equals(
        env,
        ["leaf" + FRAGMENT_SUFFIX, "root" + FRAGMENT_SUFFIX],
        sorted([f.basename for f in fragments]),
        "each fragment is named after its own target",
    )
    return analysistest.end(env)

fragments_reach_private_test = analysistest.make(
    _fragments_reach_private_impl,
    # The same application `--aspects=...%tsconfig_aspect` makes on a target
    # pattern, which is how .bazelrc turns this on for every build.
    extra_target_under_test_aspects = [tsconfig_aspect],
    attrs = {"member": attr.label(mandatory = True)},
)

def _fragment_files_impl(ctx):
    return [DefaultInfo(files = ctx.attr.dep[TsconfigFragmentInfo].fragments)]

fragment_files = rule(
    implementation = _fragment_files_impl,
    attrs = {
        "dep": attr.label(
            aspects = [tsconfig_aspect],
            mandatory = True,
            doc = "A target whose whole fragment closure becomes this target's files.",
        ),
    },
    doc = """The `ide_fragments` closure of one target, as ordinary outputs.

Their basenames are the owning target's name plus the fragment suffix, so a test
can rlocation an individual one and read the bytes a real build wrote.""",
)
