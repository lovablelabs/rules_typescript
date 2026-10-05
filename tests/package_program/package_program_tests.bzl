"""Which files of a dep a ts_test's check reads: the sources under the test's
tsconfig, the declarations under another, and a dep held as sources brings no
declarations along any path."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_action_label", "runnable_actions")
load("//ts:defs.bzl", "TsInfo")

_PKG = "tests/package_program/"

def _rel(f):
    return f.path[f.path.find(_PKG) + len(_PKG):]

def _rels(files):
    return sorted([_rel(f) for f in files])

def _label(target):
    text = str(target.label)
    return text[2:] if text.startswith("@@//") else text

def _action(env, mnemonic):
    for action in runnable_actions(env):
        if action.mnemonic == mnemonic:
            return action
    return None

def _manifest_lines(env, check):
    name = runnable_action_label(env).name
    manifest = [
        f
        for f in check.inputs.to_list()
        if f.basename == name + ".ownership"
    ]
    asserts.equals(env, 1, len(manifest), "the ownership manifest is an input")
    if len(manifest) != 1:
        return []
    writers = [
        a
        for a in runnable_actions(env)
        if manifest[0] in a.outputs.to_list()
    ]
    asserts.equals(env, 1, len(writers), "one action writes the manifest")
    if len(writers) != 1:
        return []
    return (writers[0].content or "").split("\n")

def _dep(ctx, env):
    info = ctx.attr.dep[TsInfo]
    asserts.equals(
        env,
        ["greet.mjs", "lib.ts"],
        _rels(info.sources.to_list()),
        "the dep's sources: its TypeScript and JavaScript srcs",
    )
    asserts.equals(
        env,
        ["greet.d.mts", "lib.d.ts"],
        _rels(info.declarations.to_list()),
        "the dep's declarations",
    )
    return info

def _reads_no_declaration_of(env, dep, label):
    declarations = dep.declarations.to_list()
    for action in runnable_actions(env):
        inputs = action.inputs.to_list()
        for f in declarations:
            asserts.false(
                env,
                f in inputs,
                "{} reads {}'s {}".format(action.mnemonic, label, _rel(f)),
            )
    target = analysistest.target_under_test(env)
    runfiles = target[DefaultInfo].default_runfiles.files.to_list()
    for f in declarations:
        asserts.false(env, f in runfiles, _rel(f) + " is in the runfiles")

def _checks_the_sources_impl(ctx):
    env = analysistest.begin(ctx)
    dep = _dep(ctx, env)
    asserts.equals(
        env,
        ctx.file.tsconfig,
        dep.tsconfig,
        "the dep's tsconfig is the test's",
    )
    sources = dep.sources.to_list()

    for mnemonic in ["TsConfig", "TsgoCheck"]:
        action = _action(env, mnemonic)
        asserts.true(env, action != None, "the test runs " + mnemonic)
        if action == None:
            continue
        inputs = action.inputs.to_list()
        for f in sources:
            asserts.true(
                env,
                f in inputs,
                "{} reads the dep's {}".format(mnemonic, _rel(f)),
            )
    _reads_no_declaration_of(env, dep, "the dep")

    check = _action(env, "TsgoCheck")
    if check != None:
        lines = _manifest_lines(env, check)
        label = _label(ctx.attr.dep)
        for f in sources:
            asserts.true(
                env,
                "file\t{}\t{}".format(label, f.path) in lines,
                "the manifest gives {} to {}:\n{}".format(
                    _rel(f),
                    label,
                    "\n".join(lines),
                ),
            )
            asserts.false(
                env,
                "own\t" + f.path in lines,
                "the test owns the dep's " + _rel(f),
            )
    return analysistest.end(env)

def _reads_the_declarations_impl(ctx):
    env = analysistest.begin(ctx)
    dep = _dep(ctx, env)
    asserts.false(
        env,
        ctx.file.tsconfig == dep.tsconfig,
        "the dep's tsconfig is another file",
    )
    check = _action(env, "TsgoCheck")
    asserts.true(env, check != None, "the test runs TsgoCheck")
    if check == None:
        return analysistest.end(env)
    inputs = check.inputs.to_list()
    for f in dep.declarations.to_list():
        asserts.true(env, f in inputs, "TsgoCheck reads the dep's " + _rel(f))
    for f in dep.sources.to_list():
        asserts.false(env, f in inputs, "TsgoCheck reads the dep's " + _rel(f))
    return analysistest.end(env)

def _drops_the_declarations_on_every_path_impl(ctx):
    env = analysistest.begin(ctx)
    dep = _dep(ctx, env)
    through = ctx.attr.through[TsInfo]
    asserts.equals(
        env,
        ctx.file.tsconfig,
        dep.tsconfig,
        "the dep's tsconfig is the test's",
    )
    asserts.false(
        env,
        ctx.file.tsconfig == through.tsconfig,
        "the dep between is under another tsconfig",
    )
    asserts.equals(
        env,
        ["mid.d.ts"],
        _rels(through.declarations.to_list()),
        "the dep between has declarations of its own",
    )
    check = _action(env, "TsgoCheck")
    asserts.true(env, check != None, "the test runs TsgoCheck")
    if check == None:
        return analysistest.end(env)
    inputs = check.inputs.to_list()
    for f in dep.sources.to_list():
        asserts.true(env, f in inputs, "TsgoCheck reads the dep's " + _rel(f))
    for f in through.declarations.to_list():
        asserts.true(
            env,
            f in inputs,
            "TsgoCheck reads the dep between's " + _rel(f),
        )
    _reads_no_declaration_of(env, dep, "the dep between's dep")
    return analysistest.end(env)

def _compile_reads_the_declarations_impl(ctx):
    env = analysistest.begin(ctx)
    dep = _dep(ctx, env)
    asserts.equals(
        env,
        ctx.file.tsconfig,
        dep.tsconfig,
        "the dep's tsconfig is the compile's",
    )
    check = _action(env, "TsgoCheck")
    asserts.true(env, check != None, "the compile runs TsgoCheck")
    if check == None:
        return analysistest.end(env)
    inputs = check.inputs.to_list()
    for f in dep.declarations.to_list():
        asserts.true(env, f in inputs, "TsgoCheck reads the dep's " + _rel(f))
    for f in dep.sources.to_list():
        asserts.false(env, f in inputs, "TsgoCheck reads the dep's " + _rel(f))
    return analysistest.end(env)

def _owners_record_impl(ctx):
    env = analysistest.begin(ctx)
    dep = _dep(ctx, env)
    target = analysistest.target_under_test(env)
    info = target[TsInfo]
    records = {r.label: r for r in info.owners.to_list()}
    asserts.equals(
        env,
        sorted([_label(ctx.attr.dep), _label(target)]),
        sorted(records.keys()),
        "one record per first-party target in the closure",
    )
    for label, expected in [
        (_label(target), info.declarations),
        (_label(ctx.attr.dep), dep.declarations),
    ]:
        if label not in records:
            continue
        record = records[label]
        asserts.equals(
            env,
            _rels(expected.to_list()),
            _rels(record.declarations.to_list()),
            "the record of {} carries its declarations".format(label),
        )
        files = record.files.to_list()
        for f in expected.to_list():
            asserts.true(
                env,
                f in files,
                "the record's files hold its {}".format(_rel(f)),
            )
    return analysistest.end(env)

_ATTRS = {
    "dep": attr.label(
        mandatory = True,
        doc = "The ts_compile the target under test depends on.",
    ),
    "tsconfig": attr.label(
        mandatory = True,
        allow_single_file = [".json"],
        doc = "The target under test's tsconfig.",
    ),
}

checks_the_sources_test = analysistest.make(
    _checks_the_sources_impl,
    attrs = _ATTRS,
    extra_target_under_test_aspects = [runnable_action_aspect],
)

reads_the_declarations_test = analysistest.make(
    _reads_the_declarations_impl,
    attrs = _ATTRS,
    extra_target_under_test_aspects = [runnable_action_aspect],
)

drops_the_declarations_on_every_path_test = analysistest.make(
    _drops_the_declarations_on_every_path_impl,
    attrs = _ATTRS | {
        "through": attr.label(
            mandatory = True,
            doc = "A dep under another tsconfig that depends on `dep`.",
        ),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

compile_reads_the_declarations_test = analysistest.make(
    _compile_reads_the_declarations_impl,
    attrs = _ATTRS,
    extra_target_under_test_aspects = [runnable_action_aspect],
)

owners_record_test = analysistest.make(
    _owners_record_impl,
    attrs = {
        "dep": attr.label(
            mandatory = True,
            doc = "The ts_compile the target under test depends on.",
        ),
    },
)
