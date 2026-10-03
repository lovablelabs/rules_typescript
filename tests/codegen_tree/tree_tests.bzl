"""The tree is a tsgo input, and one line of the ownership manifest.

A directory has no file list at analysis time, so the manifest names it as its
own path and the check owns every file under it by that entry.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//ts:defs.bzl", "TsInfo")

def _tree_reaches_compile_impl(ctx):
    env = analysistest.begin(ctx)

    trees = [
        f
        for f in ctx.attr.tree[TsInfo].declarations.to_list()
        if f.is_directory
    ]
    asserts.equals(env, 1, len(trees), "the codegen target provides no tree")
    tsgo = [
        a
        for a in analysistest.target_actions(env)
        if a.mnemonic == "TsgoCheck"
    ]
    asserts.equals(env, 1, len(tsgo), "one TsgoCheck action")
    if len(trees) != 1 or len(tsgo) != 1:
        return analysistest.end(env)
    tree = trees[0]
    asserts.true(
        env,
        tree in tsgo[0].inputs.to_list(),
        "the tree is no tsgo input: nothing it declares is in the program",
    )

    name = analysistest.target_under_test(env).label.name
    manifest_name = name + ".ownership"
    writers = [
        a
        for a in analysistest.target_actions(env)
        if manifest_name in [f.basename for f in a.outputs.to_list()]
    ]
    asserts.equals(env, 1, len(writers), "one action writes the manifest")
    if len(writers) == 1 and writers[0].content != None:
        lines = writers[0].content.split("\n")
        owning = [
            l
            for l in lines
            if l.startswith("file\t") and l.endswith("\t" + tree.path)
        ]
        asserts.equals(
            env,
            1,
            len(owning),
            "the manifest names the tree once, as its path:\n" +
            "\n".join(lines),
        )
    return analysistest.end(env)

tree_reaches_compile_test = analysistest.make(
    _tree_reaches_compile_impl,
    attrs = {
        "tree": attr.label(
            mandatory = True,
            doc = "The ts_codegen under test.",
        ),
    },
)

def _dev_server_declared_tree_impl(ctx):
    env = analysistest.begin(ctx)
    trees = ctx.attr.tree[TsInfo].js.to_list()
    asserts.equals(env, 1, len(trees), "the generator declares one compiled tree")
    if len(trees) != 1:
        return analysistest.end(env)
    tree = trees[0]
    asserts.true(env, tree.is_directory, "the generated input is a directory File")
    entry = ctx.attr.entry[TsInfo]
    asserts.true(env, tree in entry.transitive_js.to_list(), "the compiled tree stays live under a source-mode consumer")
    pairs = [pair for owner in entry.owners.to_list() for pair in getattr(owner, "runtime_files", ())]
    asserts.true(env, (tree, tree) in pairs, "the producer retains its exact original tree identity")
    runfiles = analysistest.target_under_test(env)[DefaultInfo].default_runfiles.files.to_list()
    asserts.true(env, tree in runfiles, "the dev server retains the original tree File")
    configs = [
        action
        for action in analysistest.target_actions(env)
        if any([output.basename == "vite.config.mjs" for output in action.outputs.to_list()])
    ]
    asserts.equals(env, 1, len(configs), "the dev server writes one plugin config")
    if len(configs) == 1:
        binding = "[{}]: {{ path: path.resolve(fs.realpathSync(bazelBin), {}), context: \"source\", isSource: false, directory: true }}".format(json.encode(tree.short_path), json.encode(tree.short_path))
        asserts.equals(env, 1, configs[0].content.count(binding), "the config retains one declared tree boundary")
    return analysistest.end(env)

dev_server_declared_tree_test = analysistest.make(
    _dev_server_declared_tree_impl,
    attrs = {
        "entry": attr.label(providers = [TsInfo], mandatory = True),
        "tree": attr.label(providers = [TsInfo], mandatory = True),
    },
)
