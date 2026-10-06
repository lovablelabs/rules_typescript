load("@rules_typescript//ts:defs.bzl", "ts_binary", "ts_codegen", "ts_compile", "ts_config", "ts_refresh_tsconfig", "ts_test")

# gazelle:exclude generated
# gazelle:exclude generate.mjs

ts_binary(
    name = "generator",
    entry_point = "generate.mjs",
    visibility = ["//ambient:__pkg__"],  # keep
)

ts_codegen(
    name = "source",
    srcs = ["names.json"],
    outs = ["generated/value.ts"],
    args = [
        "{srcs}",
        "{out}",
        "source",
    ],
    generator = ":generator",
)

ts_compile(
    name = "source_ts",
    srcs = [
        ":schema",
        ":source",
    ],
    emit = True,  # keep
    tsconfig = ":generated_config",  # keep
)

ts_compile(
    name = "generated",
    deps = [
        ":source_ts",  # keep
    ],
)

ts_codegen(
    name = "schema",
    srcs = ["names.json"],
    outs = ["generated/schema.json"],
    args = [
        "{srcs}",
        "{out}",
        "json",
    ],
    generator = ":generator",
)

ts_codegen(
    name = "declarations",
    srcs = ["names.json"],
    args = [
        "{srcs}",
        "{out}",
        "tree",
    ],
    generator = ":generator",
    out_dir = "generated/types",
    visibility = ["//authored/nested:__pkg__"],
)

ts_test(
    name = "generated_test",
    emit = True,  # keep
    runner = "@rules_typescript//ts/runners:node_test",
    deps = [
        ":source_ts",  # keep
    ],
)

ts_codegen(
    name = "config_leaf",
    srcs = ["names.json"],
    outs = ["generated/leaf.json"],
    args = [
        "{srcs}",
        "{out}",
        "config-leaf",
    ],
    generator = ":generator",
)

ts_codegen(
    name = "config_base",
    srcs = ["names.json"],
    outs = ["generated/base.json"],
    args = [
        "{srcs}",
        "{out}",
        "config-base",
    ],
    generator = ":generator",
)

# Gazelle derives source membership; these generated config contracts exercise runfiles.
ts_config(
    name = "generated_base",
    src = ":config_base",
)

ts_config(
    name = "generated_config",
    src = ":config_leaf",
    deps = [":generated_base"],
)

ts_refresh_tsconfig(
    name = "refresh_generated",
    generated_sources = True,
    tsconfig = None,
    deps = [
        ":generated",
        ":generated_test",
        ":source_ts",
        "//ambient",
        "//authored/nested",
    ],
)

ts_config(
    name = "tsconfig",
    src = "tsconfig.build.json",  # keep
)
