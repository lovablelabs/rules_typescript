"""Public API for npm dependency management in BUILD files.

Users should load from this file in their BUILD files:
    load(
        "@rules_typescript//npm:defs.bzl",
        "node_modules",
        "node_modules_member",
        "npm_bin",
    )

Note: npm repositories are declared by the npm module extension in
MODULE.bazel, not in BUILD files.  To set up npm dependencies, use:

    # In MODULE.bazel:
    npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
    npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
    use_repo(npm, "npm")
"""

load("//npm/private:npm_bin.bzl", _npm_bin = "npm_bin_macro")
load(
    "//ts/private:node_modules.bzl",
    _node_modules = "node_modules",
    _node_modules_member = "node_modules_member",
)

node_modules = _node_modules
node_modules_member = _node_modules_member
npm_bin = _npm_bin
