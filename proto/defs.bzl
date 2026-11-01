"""TypeScript source libraries generated from proto_library sources."""

load("@protobuf//bazel/common:proto_common.bzl", "proto_common")
load("@protobuf//bazel/common:proto_info.bzl", "ProtoInfo")
load("//proto/private:configuration.bzl", "proto_source_info")
load("//ts/private/rules:ts_compile.bzl", "ts_compile")

# protobuf registers its prebuilt protoc for this type, with the source build as fallback.
_PROTO_TOOLCHAIN = Label("@protobuf//bazel/private:proto_toolchain_type")

# One exec group for protoc and the plugin, so both are built for the platform the action runs on.
_PROTOC = "protoc"

# protobuf-es imports the protobuf module's schemas, the well-known types, from its runtime.
_RUNTIME_REPO = Label("@protobuf//:any_proto").repo_name

_ProtoClosureInfo = provider(
    doc = "The ProtoInfo of every proto_library a target reaches, except the protobuf module's.",
    fields = ["protos"],
)

def _closure_aspect_impl(target, ctx):
    own = [] if ctx.rule.kind == "alias" or target.label.repo_name == _RUNTIME_REPO else [target[ProtoInfo]]
    deps = []
    for attribute in ("deps", "exports", "actual"):
        value = getattr(ctx.rule.attr, attribute, [])
        deps.extend(value if type(value) == "list" else [value])
    return [_ProtoClosureInfo(protos = depset(own, transitive = [dep[_ProtoClosureInfo].protos for dep in deps if _ProtoClosureInfo in dep]))]

_closure_aspect = aspect(
    implementation = _closure_aspect_impl,
    attr_aspects = ["deps", "exports", "actual"],
    required_providers = [ProtoInfo],
)

def _compile(ctx, proto):
    paths = [proto_common.get_import_path(src).removesuffix(".proto") + "_pb.ts" for src in proto.direct_sources]
    outputs = [ctx.actions.declare_file(ctx.attr.out_dir + "/" + path) for path in paths]
    toolchain = proto_common.ProtoLangToolchainInfo(
        out_replacement_format_flag = "--es_out=%s",
        output_files = "legacy",
        plugin_format_flag = "--plugin=protoc-gen-es=%s",
        plugin = ctx.attr._plugin[DefaultInfo].files_to_run,
        proto_compiler = ctx.exec_groups[_PROTOC].toolchains[_PROTO_TOOLCHAIN].proto.proto_compiler,
        protoc_opts = ["--es_opt=" + ",".join(ctx.attr.options)],
        mnemonic = "TsProtoGenerate",
        progress_message = "Generating TypeScript protobuf sources %{label}",
    )
    proto_common.compile(
        actions = ctx.actions,
        proto_info = proto,
        proto_lang_toolchain_info = toolchain,
        generated_files = outputs,
        plugin_output = outputs[0].path.removesuffix(paths[0]),
        experimental_exec_group = _PROTOC,
    )
    return outputs

def _generate_impl(ctx):
    proto = ctx.attr.proto[0][ProtoInfo]
    if not proto.direct_sources:
        fail("ts_proto_library: %s has no direct sources. Did you mean ts_proto_closure, which takes source-less roots?" % ctx.attr.proto[0].label)
    return [DefaultInfo(files = depset(_compile(ctx, proto)))]

def _generate_closure_impl(ctx):
    # One protoc action per proto_library needs the list.
    closure = depset(transitive = [target[_ProtoClosureInfo].protos for target in ctx.attr.protos]).to_list()
    outputs = [output for proto in closure if proto.direct_sources for output in _compile(ctx, proto)]
    if not outputs:
        fail("ts_proto_closure: %s reach only protobuf's well-known types, which protobuf-es imports from its runtime" % [str(target.label) for target in ctx.attr.protos])
    return [DefaultInfo(files = depset(outputs))]

_GENERATE_ATTRS = {
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
    "out_dir": attr.string(mandatory = True),
    "options": attr.string_list(),
    "_plugin": attr.label(default = "//proto/private/bufrequest", executable = True, cfg = config.exec(_PROTOC)),
}

_generate = rule(
    implementation = _generate_impl,
    attrs = _GENERATE_ATTRS | {"proto": attr.label(mandatory = True, providers = [ProtoInfo], cfg = proto_source_info)},
    exec_groups = {_PROTOC: exec_group(toolchains = [_PROTO_TOOLCHAIN])},
)

_generate_closure = rule(
    implementation = _generate_closure_impl,
    attrs = _GENERATE_ATTRS | {"protos": attr.label_list(mandatory = True, providers = [ProtoInfo], cfg = proto_source_info, aspects = [_closure_aspect])},
    exec_groups = {_PROTOC: exec_group(toolchains = [_PROTO_TOOLCHAIN])},
)

def _check_options(options):
    if "target=ts" not in options or any([option.startswith("target=") and option != "target=ts" for option in options]):
        fail("TypeScript protobuf options must select target=ts")

def _ts_proto_config_impl(ctx):
    _check_options(ctx.attr.options)
    return []

ts_proto_config = rule(
    implementation = _ts_proto_config_impl,
    attrs = {
        "out_dir": attr.string(mandatory = True),
        "tsconfig": attr.label(mandatory = True, allow_files = True),
        "node_modules": attr.label(),
        "deps": attr.label_list(),
        "options": attr.string_list(default = ["target=ts"]),
    },
)

def _library(name, generate, sources, out_dir, tsconfig, deps, node_modules, options, **kwargs):
    _check_options(options)
    generated = name + "_generate"
    generate(
        name = generated,
        out_dir = out_dir,
        options = options,
        visibility = ["//visibility:private"],
        testonly = kwargs.get("testonly", False),
        **sources
    )
    ts_compile(
        name = name,
        srcs = [":" + generated],
        deps = deps,
        tsconfig = tsconfig,
        node_modules = node_modules,
        emit = False,
        **kwargs
    )

def ts_proto_library(name, proto, out_dir, tsconfig, deps = [], node_modules = None, options = ["target=ts"], **kwargs):
    """Generates direct proto files through the existing source-mode compiler owner."""
    _library(name, _generate, {"proto": proto}, out_dir, tsconfig, deps, node_modules, options, **kwargs)

def ts_proto_closure(name, protos, out_dir, tsconfig, deps = [], node_modules = None, options = ["target=ts"], **kwargs):
    """Generates `protos` and every proto_library they reach, except protobuf's well-known types, as one source-mode compiler owner."""
    if "bootstrap_wkt=true" in options:
        fail("ts_proto_closure skips the well-known types, so bootstrap_wkt=true would import files it never generates. Did you mean ts_proto_library?")
    _library(name, _generate_closure, {"protos": protos}, out_dir, tsconfig, deps, node_modules, options, **kwargs)
