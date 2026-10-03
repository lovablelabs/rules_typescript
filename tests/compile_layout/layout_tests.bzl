"""Analysis-time coverage for the layout ts_compile gives a multi-directory target.

The go_test next to this file reads the files that were actually written. These
tests read what the rule declared and told oxc, which is where a
single-common-directory assumption shows up first: one --strip-dir-prefix has to
be the package, not the directory of whichever src sorted first.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//ts:defs.bzl", "TsInfo")

_PKG = "tests/compile_layout"

def _package_relative(f):
    marker = _PKG + "/"
    return f.path[f.path.find(marker) + len(marker):]

def _declared_outputs_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)

    asserts.equals(
        env,
        [
            "alpha/one.js",
            "alpha/one.js.map",
            "alpha/three.js",
            "alpha/three.js.map",
            "beta/deep/two.js",
            "beta/deep/two.js.map",
        ],
        sorted([
            _package_relative(f)
            for f in target[DefaultInfo].files.to_list()
        ]),
        "the default outputs",
    )
    asserts.equals(
        env,
        [
            "alpha/one.d.ts",
            "alpha/one.d.ts.map",
            "alpha/three.d.ts",
            "alpha/three.d.ts.map",
            "beta/deep/two.d.ts",
            "beta/deep/two.d.ts.map",
        ],
        sorted([
            _package_relative(f)
            for f in target[OutputGroupInfo].declarations.to_list()
        ]),
        "the declarations output group",
    )
    return analysistest.end(env)

declared_outputs_test = analysistest.make(
    _declared_outputs_impl,
    config_settings = {str(Label("//ts:declaration_map")): True},
)

def _emit_root_impl(ctx):
    env = analysistest.begin(ctx)
    emit_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "TsEmit"
    ]

    # Two sibling directories are one source root, so one root.
    asserts.equals(env, 1, len(emit_actions), "TsEmit actions")
    if len(emit_actions) != 1:
        return analysistest.end(env)

    argv = emit_actions[0].argv
    asserts.equals(
        env,
        ["-root=" + _PKG],
        [a for a in argv if a.startswith("-root=")],
        "-root",
    )
    out_dirs = [a for a in argv if a.startswith("-out_dir=")]
    asserts.true(
        env,
        len(out_dirs) == 1 and out_dirs[0].endswith("/" + _PKG),
        "-out_dir is the package's bin directory: " + str(out_dirs),
    )
    return analysistest.end(env)

emit_root_test = analysistest.make(_emit_root_impl)

def _action(env, mnemonic):
    for action in analysistest.target_actions(env):
        if action.mnemonic == mnemonic:
            return action
    return None

_DATA_SRCS = [
    "gamma/README.md",
    "gamma/data.json",
    "gamma/logo.svg",
    "gamma/package.json",
    "gamma/styles.css",
]

def _data_srcs_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)

    files = target[DefaultInfo].files.to_list()
    asserts.equals(
        env,
        [
            "gamma/README.md",
            "gamma/data.json",
            "gamma/index.js",
            "gamma/index.js.map",
            "gamma/logo.svg",
            "gamma/package.json",
            "gamma/styles.css",
        ],
        sorted([_package_relative(f) for f in files]),
        "DefaultInfo: the compiled module and every data src",
    )
    asserts.equals(
        env,
        [],
        [_package_relative(f) for f in files if f.is_source],
        "every file in DefaultInfo is staged under bazel-bin",
    )

    info = target[TsInfo]
    asserts.equals(
        env,
        _DATA_SRCS,
        sorted([_package_relative(f) for f in info.data.to_list()]),
        "TsInfo.data",
    )
    asserts.equals(
        env,
        _DATA_SRCS,
        sorted([
            _package_relative(f)
            for f in info.transitive_data.to_list()
        ]),
        "TsInfo.transitive_data on a target without deps",
    )

    tsgo = _action(env, "TsgoCheck")
    asserts.true(env, tsgo != None, "ts_compile runs no TsgoCheck")
    if tsgo != None:
        asserts.equals(
            env,
            ["gamma/data.json", "gamma/index.ts", "gamma/package.json"],
            sorted([
                _package_relative(f)
                for f in tsgo.inputs.to_list()
                if not f.is_directory and "/gamma/" in f.path
            ]),
            "the tsgo action's inputs under gamma/",
        )

    # `include` is the program's root files; a JSON is reached by import.
    config = _action(env, "TsConfig")
    asserts.true(env, config != None, "ts_compile runs no TsConfig action")
    if config != None:
        asserts.equals(
            env,
            [_PKG + "/gamma/index.ts"],
            [arg for arg in config.argv if arg.startswith(_PKG + "/")],
            "the tsconfig step's root files",
        )
    return analysistest.end(env)

data_srcs_test = analysistest.make(_data_srcs_impl)

def _transitive_data_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    asserts.equals(
        env,
        [],
        info.data.to_list(),
        "a consumer stages no data of its own",
    )
    asserts.equals(
        env,
        _DATA_SRCS,
        sorted([
            _package_relative(f)
            for f in info.transitive_data.to_list()
        ]),
        "TsInfo.transitive_data carries the dep's data srcs",
    )
    return analysistest.end(env)

transitive_data_test = analysistest.make(_transitive_data_impl)

def _dep_json_inputs_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[TsInfo]
    compiler_scopes = [
        f
        for owner in info.owners.to_list()
        for f in owner.type_inputs.to_list()
        if f.path == _PKG + "/gamma/package.json"
    ]
    runtime_scopes = [f for f in info.transitive_data.to_list() if _package_relative(f) == "gamma/package.json"]
    asserts.equals(env, [True], [f.is_source for f in compiler_scopes], "one original compiler scope File")
    asserts.equals(env, [False], [f.is_source for f in runtime_scopes], "one runtime scope projected into bin")
    runtime_data = [f for f in info.transitive_data.to_list() if _package_relative(f) == "gamma/data.json"]
    asserts.equals(env, [False], [f.is_source for f in runtime_data], "one imported JSON module staged into bin")

    tsgo = _action(env, "TsgoCheck")
    asserts.true(env, tsgo != None, "ts_compile runs no TsgoCheck")
    if tsgo != None:
        inputs = [f for f in tsgo.inputs.to_list() if not f.is_directory and "/gamma/" in f.path]
        asserts.equals(
            env,
            compiler_scopes,
            [f for f in inputs if f.basename == "package.json" and f.is_source],
            "the compiler retains the original metadata File",
        )
        asserts.equals(
            env,
            runtime_scopes,
            [f for f in inputs if f.basename == "package.json" and not f.is_source],
            "the compiler also reads the exact runtime projection",
        )
        asserts.equals(
            env,
            ["gamma/data.json", "gamma/index.d.ts"],
            sorted([_package_relative(f) for f in inputs if f.basename != "package.json"]),
            "the consumer reads imported JSON and declarations, not unrelated assets",
        )
        asserts.equals(
            env,
            runtime_data,
            [f for f in inputs if _package_relative(f) == "gamma/data.json"],
            "the imported JSON retains its runtime File identity",
        )
    return analysistest.end(env)

dep_json_inputs_test = analysistest.make(_dep_json_inputs_impl)

def _sibling_scope_inputs_impl(ctx):
    env = analysistest.begin(ctx)
    scope = _PKG + "/sibling_scope/lib/package.json"
    tsgo = _action(env, "TsgoCheck")
    asserts.true(env, tsgo != None, "ts_compile runs no TsgoCheck")
    if tsgo != None:
        asserts.true(
            env,
            "-source=" + scope in tsgo.argv,
            "an emitted sibling owner's borrowed declarations resolve '#contract' only through its original scope",
        )
    return analysistest.end(env)

sibling_scope_inputs_test = analysistest.make(_sibling_scope_inputs_impl)

def _joined_scope_inputs_impl(ctx):
    env = analysistest.begin(ctx)
    tsgo = _action(env, "TsgoCheck")
    asserts.true(env, tsgo != None, "the joined program runs no TsgoCheck")
    if tsgo != None:
        for source in ["lib.ts", "package.json"]:
            asserts.true(
                env,
                "-source=" + _PKG + "/joined_scope/" + source in tsgo.argv,
                "a joined consumer compiles its owner's sources under their original scope: " + source,
            )
    return analysistest.end(env)

joined_scope_inputs_test = analysistest.make(_joined_scope_inputs_impl)
