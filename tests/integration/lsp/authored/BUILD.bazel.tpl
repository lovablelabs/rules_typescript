load("@rules_typescript//ts:defs.bzl", "ts_compile")

ts_compile(
    name = "authored",
    emit = True,  # keep
)
