load("@rules_typescript//ts:defs.bzl", "ts_codegen", "ts_compile")

ts_codegen(
    name = "ambient_declarations",
    srcs = ["ambient.json"],
    args = [
        "{srcs}",
        "{out}",
        "ambient",
    ],
    generator = "//generated:generator",
    out_dir = "src/generated",
)

# Gazelle treats the tree as generated; keep this authored collision to test precedence.
ts_compile(
    name = "ambient",
    srcs = [
        "src/generated/authored.d.ts",  # keep
    ],
)
