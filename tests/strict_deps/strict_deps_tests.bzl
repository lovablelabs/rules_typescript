"""What the compiler READS against what an import may RESOLVE to, at analysis.

A dep's own deps stay tsgo's inputs: the compiler has to read them to check the
declarations of the dep that WAS declared. The ownership manifest attributes
each of them to the label that stages it, so an edge into one is a finding
naming that label -- the drift this check exists to catch.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//ts:defs.bzl", "TsInfo")
load("//ts/private:providers.bzl", "NpmPackageInfo")

def _tsgo_action(env):
    tsgo = [
        a
        for a in analysistest.target_actions(env)
        if a.mnemonic == "TsgoCheck"
    ]
    asserts.equals(env, 1, len(tsgo), "one TsgoCheck action")
    return tsgo[0] if len(tsgo) == 1 else None

def _manifest_lines(env, tsgo):
    """The ownership manifest's lines, read off the action that writes it."""
    name = analysistest.target_under_test(env).label.name
    manifest = [
        f
        for f in tsgo.inputs.to_list()
        if f.basename == name + ".ownership"
    ]
    asserts.equals(env, 1, len(manifest), "the ownership manifest is an input")
    if len(manifest) != 1:
        return None
    asserts.true(
        env,
        "-check=" + manifest[0].path in tsgo.argv,
        "tsaction reads the manifest: " + str(tsgo.argv),
    )
    writers = [
        a
        for a in analysistest.target_actions(env)
        if manifest[0] in a.outputs.to_list()
    ]
    asserts.equals(env, 1, len(writers), "one action writes the manifest")
    if len(writers) != 1:
        return None
    asserts.true(env, writers[0].content != None, "the manifest has content")
    return (writers[0].content or "").split("\n")

def _resolution_surface_impl(ctx):
    env = analysistest.begin(ctx)
    transitive_only = [
        f.path
        for f in ctx.attr.transitive_dep[TsInfo].declarations.to_list()
    ]
    asserts.true(
        env,
        len(transitive_only) > 0,
        "the transitive dep has no declarations: a vacuous pass",
    )
    tsgo = _tsgo_action(env)
    if not tsgo:
        return analysistest.end(env)
    inputs = [f.path for f in tsgo.inputs.to_list()]
    for path in transitive_only:
        asserts.true(env, path in inputs, "tsgo does not read " + path)
    lines = _manifest_lines(env, tsgo)
    if lines == None:
        return analysistest.end(env)
    direct = [l.split("\t")[1] for l in lines if l.startswith("direct\t")]
    for path in transitive_only:
        owners = [
            l.split("\t")[1]
            for l in lines
            if l.startswith("file\t") and l.split("\t")[2] == path
        ]
        asserts.equals(env, 1, len(owners), "one label owns " + path)
        for label in owners:
            asserts.false(
                env,
                label in direct,
                "{} owns {} and is a direct dep".format(label, path),
            )
    return analysistest.end(env)

resolution_surface_test = analysistest.make(
    _resolution_surface_impl,
    attrs = {
        "transitive_dep": attr.label(
            mandatory = True,
            doc = "Reached only through another dep.",
        ),
    },
)

def _tsgo_check_impl(ctx):
    env = analysistest.begin(ctx)
    tsgo = _tsgo_action(env)
    if not tsgo:
        return analysistest.end(env)
    argv = tsgo.argv
    asserts.true(env, "--explainFiles" in argv, "tsgo lists the edges")
    asserts.true(
        env,
        "--pretty" in argv and argv[argv.index("--pretty") + 1] == "false",
        "the listing is plain text",
    )
    lines = _manifest_lines(env, tsgo)
    if lines == None:
        return analysistest.end(env)
    expected = list(ctx.attr.lines)
    for dep in ctx.attr.npm_direct:
        info = dep[NpmPackageInfo]
        expected.append("npm-direct\t{}\t{}".format(info.package_name, info.store.tree.path))
    for dep, label in ctx.attr.npm_reachable.items():
        info = dep[NpmPackageInfo]
        expected.append("npm\t{}\t{}\t{}".format(info.package_name, label, info.store.tree.path))
    for line in expected:
        asserts.true(
            env,
            line in lines,
            "the manifest carries %r:\n%s" % (line, "\n".join(lines)),
        )
    return analysistest.end(env)

tsgo_check_test = analysistest.make(
    _tsgo_check_impl,
    attrs = {
        "npm_direct": attr.label_list(providers = [NpmPackageInfo]),
        "npm_reachable": attr.label_keyed_string_dict(providers = [NpmPackageInfo]),
        "lines": attr.string_list(
            doc = "Lines the ownership manifest holds, tab-separated fields.",
        ),
    },
)
