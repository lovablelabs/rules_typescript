"""TypeScript source libraries generated from proto_library sources."""

load("@protobuf//bazel/common:proto_common.bzl", "proto_common")
load("@protobuf//bazel/common:proto_info.bzl", "ProtoInfo")
load("//proto/private:configuration.bzl", "proto_source_info")
load("//ts/private/rules:ts_compile.bzl", "ts_compile")

# protobuf registers its prebuilt protoc for this type, with the source build as fallback.
_PROTO_TOOLCHAIN = Label("@protobuf//bazel/private:proto_toolchain_type")

# One exec group for protoc and the plugin, so both are built for the platform the action runs on.
_PROTOC = "protoc"

def _generate_impl(ctx):
    proto = ctx.attr.proto[0][ProtoInfo]
    if not proto.direct_sources:
        fail("ts_proto_library: proto must have direct sources")
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
    return [DefaultInfo(files = depset(outputs))]

_generate = rule(
    implementation = _generate_impl,
    attrs = {
        "proto": attr.label(mandatory = True, providers = [ProtoInfo], cfg = proto_source_info),
        "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
        "out_dir": attr.string(mandatory = True),
        "options": attr.string_list(),
        "_plugin": attr.label(default = "//proto/private:protoc_gen_es", executable = True, cfg = config.exec(_PROTOC)),
    },
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

def ts_proto_library(name, proto, out_dir, tsconfig, deps = [], node_modules = None, options = ["target=ts"], **kwargs):
    """Generates direct proto files through the existing source-mode compiler owner."""
    _check_options(options)
    generated = name + "_generate"
    _generate(
        name = generated,
        proto = proto,
        out_dir = out_dir,
        options = options,
        visibility = ["//visibility:private"],
        testonly = kwargs.get("testonly", False),
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
