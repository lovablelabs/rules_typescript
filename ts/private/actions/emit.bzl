"""The TsEmit action: tsaction writes the program's JavaScript.

oxc transforms an ES-module program's sources, one run per root; tsgo emits a
CommonJS-shaped one, with --noCheck, from the program root the check runs in.
Under `es_modules` the emit is oxc's whatever the module, with oxc's inputs
alone. docs/rules/ts-compile.md § The Module Format.
"""

load("//ts/private:toolchain.bzl", "get_tools_toolchain")
load(":manifest.bzl", "runtime_scope_inputs")
load(":tsgo.bzl", "add_overlays", "compiler_sources")

def emit_action(
        ctx,
        oxc,
        tsgo,
        srcs,
        roots,
        outputs,
        out_base,
        tsconfig,
        chain,
        importers,
        overlays,
        manifests,
        program_inputs,
        dep_dts,
        npm_files,
        options_file,
        scratch,
        source_map,
        emit_dts,
        declared_module,
        es_modules = False,
        runtime_scopes = [],
        declarations_only = False,
        declaration_map = False,
        checkers = 0,
        generated_srcs = [],
        inherited_importers = []):
    """The configuration action validates declared_module before emission runs."""
    args = ctx.actions.args()
    args.use_param_file("@%s", use_always = False)
    args.set_param_file_format("multiline")
    args.add(options_file, format = "-options=%s")

    full_program = declarations_only or (not es_modules and bool(declared_module))
    if full_program:
        args.add(tsconfig, format = "-tsconfig=%s")
        compiler_sources(args, program_inputs, chain, dep_dts, generated_srcs)
        args.add_all(importers, format_each = "-node_modules=%s")
        args.add_all(inherited_importers, format_each = "-inherit_node_modules=%s")
        add_overlays(args, overlays)
        args.add_all(manifests, format_each = "-manifest=%s")
        args.add("-scratch=" + scratch)
    args.add("-out_dir=" + out_base)
    oxc_emits = not full_program
    if oxc_emits:
        args.add(oxc.oxc_binary, format = "-oxc=%s")
    if full_program:
        args.add(tsgo.tsgo_binary, format = "-tsgo=%s")
    args.add_all(roots, format_each = "-root=%s")
    if source_map:
        args.add("-source_map")
    if emit_dts:
        args.add("-declarations")
    if es_modules:
        args.add("-es_modules")
    if declarations_only:
        args.add("-declarations_only")
    if declaration_map:
        args.add("-declaration_map")
    if checkers > 0:
        args.add("-checkers=" + str(checkers))
    scope_inputs = runtime_scope_inputs(args, runtime_scopes)
    args.add_all(srcs)
    if not full_program:
        inputs = depset(srcs + [options_file] + scope_inputs)
    else:
        inputs = depset(
            program_inputs + [options_file, tsconfig] + chain + scope_inputs,
            transitive = [dep_dts, npm_files, tsgo.files],
        )
    tools = [oxc.oxc_binary] if oxc_emits else []
    mnemonic = "TsgoDeclare" if declarations_only else "TsEmit"
    ctx.actions.run(
        inputs = inputs,
        outputs = outputs,
        executable = get_tools_toolchain(ctx).tsaction,
        tools = tools,
        arguments = ["emit", args],
        mnemonic = mnemonic,
        progress_message = mnemonic + " %{label}",
        execution_requirements = {"cpu:{}".format(checkers): ""} if checkers > 0 else {},
    )
