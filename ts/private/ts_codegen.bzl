"""General-purpose code generation rule for TypeScript projects.

ts_codegen runs an executable generator that reads source files and produces
generated TypeScript source files as Bazel action outputs.

It is the TypeScript equivalent of proto_library → go_proto_library:
  - Source files go in (via srcs)
  - Generated .ts files come out (via outs or out_dir)
  - An executable generator transforms the sources

Output declaration:
  - outs: explicit list of output file names (use for single-file or known multi-file output)
  - out_dir: directory name for generators that produce a file tree (e.g. Prisma)

The generator binary is run as a Bazel build action (not at analysis time), so:
  - All outputs are declared at analysis time via the outs attr
  - The action is fully hermetic and cacheable
  - A generated .ts is a src of the ts_compile that compiles it; a generated
    .d.ts or .js is a dep, reached through the consumer's tsconfig (`types`
    for a declaration, `paths` for an out_dir tree)

Typical patterns:

  1. Node.js generator (the usual shape):
     A ts_binary whose entry_point is the script itself.  The rule resolves the
     runtime from the JS runtime toolchain, so nothing reads NODE_BINARY.

         ts_binary(name = "mygen", entry_point = "mygen.mjs")
         ts_codegen(
             name = "gen",
             srcs = ["input.ts"],
             outs = ["generated.ts"],
             generator = ":mygen",
             args = ["--out", "{out}"],
         )

     node_modules on the ts_codegen, the importer's `node_modules` target,
     puts NODE_PATH and TS_CODEGEN_NODE_MODULES in the generator's
     environment, which is how the script reaches npm packages.  A workspace
     member the generator resolves is the importer's link target in deps,
     `:node_modules/<member name>`.

         ts_binary(name = "gen_schema", entry_point = "generate-schema.mjs")
         ts_codegen(
             name = "schema",
             srcs = ["schema.json"],
             outs = ["schema.gen.ts"],
             generator = ":gen_schema",
             args = ["--in", "{srcs}", "--out", "{out}"],
             node_modules = ":node_modules",
         )

  2. Any other executable:
     A generator that is not a Node program -- a Go binary, a shell script --
     is any executable target.  A shell wrapper that does run node reads
     $NODE_BINARY, which ts_codegen sets from the js_tool toolchain.

         sh_binary(name = "legacy_gen", srcs = ["legacy_gen.sh"])

  3. A generator this ruleset ships:
     //tools/codegen:tanstack_routes writes a TanStack Router route tree, and
     //tools/codegen:wrangler_types a worker's worker-configuration.d.ts.

         ts_codegen(
             name = "route_tree",
             srcs = glob(["**/*.tsx"]),
             outs = ["routeTree.gen.expected.ts"],
             generator = "@rules_typescript//tools/codegen:tanstack_routes",
             args = ["--out", "{out}", "--srcs", "{srcs}"],
             node_modules = "//:node_modules",
         )

     A route tree has to be checked in -- the routes are typed against it, and
     one ts_compile cannot hold both it and them -- so a diff_test beside the
     target fails when the checked-in copy drifts.

Placeholder substitution in args:
  {srcs_dir}         → execroot-relative directory of the first src file
  {out}              → execroot-relative path of the first declared output
  {outs_dir}         → execroot-relative directory of the first declared output
  {srcs}             → space-separated list of all src file paths
  {node_modules_dir} → execroot-relative path of the importer's node_modules
                       directory (only valid when node_modules is set)

When node_modules is set, ts_codegen automatically sets:
  NODE_PATH              → node_modules directory (for Node.js CJS resolution)
  TS_CODEGEN_NODE_MODULES → same path (for scripts that fork child processes)
"""

load(
    "//ts/private:providers.bzl",
    "NodeModulesInfo",
    "NpmLinkInfo",
    "ts_info",
)
load("//ts/private:runtime.bzl", "JS_TOOL_TOOLCHAIN_TYPE", "get_js_tool")

_DECLARATION_SUFFIXES = (".d.ts", ".d.mts", ".d.cts")

_JS_EXTENSIONS = ["js", "mjs", "cjs"]

# ─── Rule implementation ───────────────────────────────────────────────────────

def _ts_codegen_impl(ctx):
    # Collect all source files.
    srcs = ctx.files.srcs
    if not srcs:
        fail("ts_codegen: srcs must not be empty")

    # Validate that exactly one of outs or out_dir is set.
    has_outs = len(ctx.outputs.outs) > 0
    has_out_dir = ctx.attr.out_dir != ""
    if not has_outs and not has_out_dir:
        fail("ts_codegen: either outs or out_dir must be set")
    if has_outs and has_out_dir:
        fail("ts_codegen: outs and out_dir are mutually exclusive; set exactly one")

    # Collect declared output files (or declare a directory).
    if has_out_dir:
        # Directory output: declare a single directory for generators that
        # produce a tree of files (e.g. Prisma client generation).
        out_dir_file = ctx.actions.declare_directory(ctx.attr.out_dir)
        outs = [out_dir_file]
    else:
        outs = ctx.outputs.outs

    # Compute placeholder values.
    # {srcs_dir}: directory of the first source file (execroot-relative).
    srcs_dir = srcs[0].dirname if srcs else ""

    # {out}: path of the first output file (execroot-relative).
    out_path = outs[0].path if outs else ""

    # {outs_dir}: directory of the first output file.
    outs_dir = outs[0].dirname if outs else ""

    # {srcs}: space-separated list of all source paths.
    srcs_list = " ".join([f.path for f in srcs])

    node_modules_files = depset()
    node_modules_dir = ""
    if ctx.attr.node_modules:
        node_modules_files = ctx.attr.node_modules[DefaultInfo].files
        node_modules_dir = ctx.attr.node_modules[NodeModulesInfo].dir
    member_links = [dep[NpmLinkInfo] for dep in ctx.attr.deps]
    member_files = depset(
        [entry.link for entry in member_links],
        transitive = [entry.store.transitive for entry in member_links],
    )

    # Resolve node as a build tool (for passing NODE_BINARY env).
    js_tool = get_js_tool(ctx)
    runtime_binary = js_tool.runtime_binary if js_tool else None

    # Expand placeholders in each argument string.
    expanded_args = []
    for arg in ctx.attr.args:
        a = arg
        a = a.replace("{srcs_dir}", srcs_dir)
        a = a.replace("{out}", out_path)
        a = a.replace("{outs_dir}", outs_dir)
        a = a.replace("{srcs}", srcs_list)
        if node_modules_dir:
            a = a.replace("{node_modules_dir}", node_modules_dir)
        expanded_args.append(a)

    # Build the action environment.
    action_env = {}
    action_env.update(ctx.attr.env)

    # When node_modules is provided, automatically set NODE_PATH so that the
    # generator script can import npm packages without knowing their path.
    # Also expose the path via TS_CODEGEN_NODE_MODULES for scripts that fork.
    if node_modules_dir:
        action_env["NODE_PATH"] = node_modules_dir
        action_env["TS_CODEGEN_NODE_MODULES"] = node_modules_dir

    # When node is available from the js_tool toolchain, expose its path via
    # NODE_BINARY so generator shell scripts can invoke `$NODE_BINARY script.mjs`
    # without relying on `node` being in PATH.
    extra_inputs = []
    if runtime_binary:
        action_env.setdefault("NODE_BINARY", runtime_binary.path)
        extra_inputs.append(runtime_binary)

    inputs = depset(
        srcs + extra_inputs,
        transitive = [node_modules_files, member_files],
    )

    # Run the generator action.
    ctx.actions.run(
        executable = ctx.executable.generator,
        arguments = expanded_args,
        inputs = inputs,
        outputs = outs,
        env = action_env,
        mnemonic = "TsCodegen",
        progress_message = "TsCodegen %{label}",
    )

    # A tree is compiled output whole; scalar .d.ts, .js and .json are a dep's,
    # while a .ts out is a source for a consumer's srcs.
    files = depset(outs)
    if has_out_dir:
        js, declarations = files, files
    else:
        js = depset([f for f in outs if f.extension in _JS_EXTENSIONS])
        declarations = depset([f for f in outs if f.basename.endswith(_DECLARATION_SUFFIXES)])
    data = depset([f for f in ctx.outputs.outs if f.extension == "json"], order = "postorder")
    return [
        DefaultInfo(files = files),
        ts_info(js = js, declarations = declarations, data = data, label = ctx.label),
    ]

# ─── Rule declaration ──────────────────────────────────────────────────────────

ts_codegen = rule(
    implementation = _ts_codegen_impl,
    attrs = {
        "srcs": attr.label_list(
            doc = "Source files read by the generator (e.g. route files, schema files).",
            allow_files = True,
            mandatory = True,
        ),
        "outs": attr.output_list(
            doc = """Declared output files produced by the generator.

All outputs must be declared at analysis time — Bazel requires this.
The generator must write exactly the files listed here.

Mutually exclusive with out_dir. Use out_dir when the generator
produces a directory tree instead of individual files.
""",
        ),
        "out_dir": attr.string(
            doc = """Output directory name for generators that produce a tree of files.

When set, a single Bazel declared directory is created at this path.
The generator is expected to write all of its outputs under this directory.

Use this for generators like Prisma that produce many files in a tree
(e.g. generated/client/index.ts, generated/client/schema.prisma, ...).

Mutually exclusive with outs. The {out} and {outs_dir} placeholders in
args resolve to the declared directory path when out_dir is used.

A consumer reaches the tree by relative path or through a `paths` entry in its
tsconfig whose value names this directory; the bin-dir twin of that value is
where the tree is.
""",
            default = "",
        ),
        "generator": attr.label(
            doc = """Executable that produces the generated files.

Typically an sh_binary wrapping a shell script or a Node.js script.
Built for the exec configuration (build machine) so it runs as a Bazel action.

When the generator is a Node.js script that imports npm packages, wrap it in
sh_binary and rely on the NODE_BINARY env variable (set automatically by
ts_codegen when the js_tool toolchain is registered) to invoke node.  Locate the
script with a runfiles library, not "$0.runfiles" -- that directory does not
exist when Bazel hands the action a runfiles manifest instead of a tree:

    #!/usr/bin/env bash
    # source @bazel_tools//tools/bash/runfiles first; it defines rlocation.
    exec "$NODE_BINARY" "$(rlocation _main/path/to/script.mjs)" "$@"
""",
            mandatory = True,
            executable = True,
            cfg = "exec",
        ),
        "args": attr.string_list(
            doc = """Command-line arguments passed to the generator.

Supports placeholder substitution:
  {srcs_dir}         → execroot-relative directory of the first src file
  {out}              → execroot-relative path of the first declared output file
  {outs_dir}         → execroot-relative directory of the first declared output
  {srcs}             → space-separated list of all src file paths
  {node_modules_dir} → execroot-relative path of the importer's node_modules
                       directory (only valid when node_modules is set)

Example:
    args = ["--routes-dir", "{srcs_dir}", "--out", "{out}"]
""",
            default = [],
        ),
        "node_modules": attr.label(
            doc = """The importer's `node_modules` target, for a generator that
imports npm packages at runtime.

When set:
  - Every link of the directory and every store tree it reaches is an input
  - NODE_PATH is set to the directory (for CJS resolution)
  - TS_CODEGEN_NODE_MODULES is set to the same path
  - {node_modules_dir} placeholder is available in args
""",
            providers = [NodeModulesInfo],
        ),
        "deps": attr.label_list(
            doc = """The workspace members the generator resolves, as the
importer's link targets, `//<importer>:node_modules/<name>`.

Each link and the member's store tree join the action's inputs; the link sits
in the directory `node_modules` names, where the walk up from a file under the
importer finds it. A published package is the importer's to link in
`node_modules`.
""",
            providers = [[NpmLinkInfo]],
        ),
        "env": attr.string_dict(
            doc = "Additional environment variables passed to the generator action.",
            default = {},
        ),
    },
    toolchains = [
        config_common.toolchain_type(JS_TOOL_TOOLCHAIN_TYPE, mandatory = False),
    ],
    doc = """Runs a generator executable to produce TypeScript source files from inputs.

ts_codegen is the TypeScript equivalent of proto_library -> go_proto_library:
it runs an executable generator that reads source files and produces generated
.ts files which can then be compiled with ts_compile.

See module docstring for invocation patterns and placeholder substitution.
""",
)
