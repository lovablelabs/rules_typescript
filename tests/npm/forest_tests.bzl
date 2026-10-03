"""Analysis-time proof that tsgo resolves npm packages through the importer
chain. The build tests beside this prove the programs resolve; these pin the
route: `-node_modules=` names the chain's directories nearest first,
`-overlay=` the first-party deps at or above the package, `-manifest=` their
package.json as built, the action's inputs
are the chain's links for the direct names with the closure's store trees and
the edge links beside them, no tree is built per target, and no npm file is
staged from a source repository. The tsconfig step is handed the direct @types
deps and nothing about the closure."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")

def _action(env, mnemonic):
    for action in runnable_actions(env):
        if action.mnemonic == mnemonic:
            return action
    return None

def _chain_impl(ctx):
    env = analysistest.begin(ctx)
    tsgo = _action(env, "TsgoCheck")
    asserts.true(env, tsgo != None, "ts_compile runs no TsgoCheck")
    if tsgo == None:
        return analysistest.end(env)
    asserts.equals(
        env,
        [ctx.bin_dir.path + "/" + d for d in ctx.attr.chain],
        [
            arg[len("-node_modules="):]
            for arg in tsgo.argv
            if arg.startswith("-node_modules=")
        ],
        "the importer chain, nearest first",
    )
    overlays = [json.decode(arg[len("-overlay="):]) for arg in tsgo.argv if arg.startswith("-overlay=")]
    package_overlays = [[ctx.bin_dir.path + "/" + directory, directory] for directory in ctx.attr.overlays]
    asserts.equals(env, package_overlays, overlays[:len(package_overlays)], "first-party package outputs overlay their original logical directories")
    input_paths = {file.path: True for file in tsgo.inputs.to_list()}
    for physical, logical in overlays[len(package_overlays):]:
        asserts.true(env, physical in input_paths, "an exact declaration or JSON overlay reads its published File")
        asserts.equals(env, physical[len(ctx.bin_dir.path) + 1:] if physical.startswith(ctx.bin_dir.path + "/") else physical, logical, "this unmoved dependency retains its source-origin path")
    asserts.equals(
        env,
        [ctx.bin_dir.path + "/" + m for m in ctx.attr.manifests],
        [
            arg[len("-manifest="):]
            for arg in tsgo.argv
            if arg.startswith("-manifest=")
        ],
        "those deps' manifests as built, each laid at its package's path",
    )
    inputs = tsgo.inputs.to_list()
    asserts.equals(
        env,
        [],
        [
            f.path
            for f in inputs
            if f.is_directory and "/node_modules/.pnpm/" not in f.path
        ],
        "every tree input is a store tree",
    )
    asserts.equals(
        env,
        [],
        [f.path for f in inputs if f.is_source and "/node_modules/" in f.path],
        "no npm file is staged from a source repository",
    )
    staged = [f.short_path for f in inputs]
    for rel in ctx.attr.linked:
        asserts.true(
            env,
            len([p for p in staged if p == rel or p.endswith("/" + rel)]) > 0,
            "the action stages " + rel,
        )

    config = _action(env, "TsConfig")
    asserts.true(env, config != None, "ts_compile writes its tsconfig in no action")
    if config != None:
        asserts.equals(
            env,
            ctx.attr.types_deps,
            [
                arg[len("-types_dep="):]
                for arg in config.argv
                if arg.startswith("-types_dep=")
            ],
            "the tsconfig step is handed the direct @types deps and nothing else",
        )
    return analysistest.end(env)

chain_test = analysistest.make(
    _chain_impl,
    extra_target_under_test_aspects = [runnable_action_aspect],
    attrs = {
        "chain": attr.string_list(
            doc = "The importers' node_modules directories, bin-dir " +
                  "relative, nearest first.",
        ),
        "linked": attr.string_list(
            doc = "Path suffixes the tsgo action stages: an importer's link " +
                  "`node_modules/<name>`, a store tree, an edge beside one.",
        ),
        "overlays": attr.string_list(
            doc = "The packages of the first-party deps at or above the " +
                  "target's, bin-dir relative, whose outputs the program " +
                  "root lays over the sources.",
        ),
        "manifests": attr.string_list(
            doc = "Those deps' package.json as built, bin-dir relative, " +
                  "each laid at its package's package.json.",
        ),
        "types_deps": attr.string_list(
            doc = "The @types packages the target declares directly, by the " +
                  "name a `types` entry uses.",
        ),
    },
)
