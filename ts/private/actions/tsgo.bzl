"""Tsgo validation and shared compiler-program inputs.

tsaction runs tsgo from a program root holding the action's source inputs at
their paths and the output tree whole, with each importer's node_modules at
the importer's directory and the declarations, manifest and data of each
first-party dep at or above the target's package laid over that package's
sources -- never the dep's JavaScript, which a program reads through the
declarations -- the dep's package.json as built at the package's path, so the
tsconfig's include names the target's srcs and the deps' declarations and
every bare specifier resolves as over pnpm's install, the package's own name
included. TsgoCheck runs --noEmit over the written tsconfig, which
carries no emit shape, and checks the --explainFiles listing against the
ownership manifest written here: an edge from one of the target's files into
a file a label outside deps owns fails the action naming that label
(docs/rules/ts-compile.md § Deps Have to Be Direct), and so does a src the
listing lacks that the chain's exclude names.
"""

load("//ts/private:providers.bzl", "label_text")
load("//ts/private:toolchain.bzl", "get_tools_toolchain")
load(":manifest.bzl", "runtime_scope_inputs")

def npm_hub_label(npm_info, package = ""):
    """The hub label a deps list writes for an npm package: the root's
    resolution, or the importer `package`'s.

    The closure carries NpmPackageInfo, not labels: a transitive package was
    never named in any deps list here. Its own repository is
    `<hub>__<package>__<version>...`, so the hub the extension created -- which
    is what a deps list names -- is recoverable from the file it provides.
    """
    name = npm_info.package_name
    label_name = name[1:].replace("/", "_") if name.startswith("@") else name
    hub = "npm"
    owner = npm_info.package_dir.owner if npm_info.package_dir else None
    if owner and owner.repo_name:
        candidate = owner.repo_name.split("__")[0].split("+")[-1]
        if candidate:
            hub = candidate
    return "@{}//{}:{}".format(hub, package, label_name)

def npm_hub_entry(npm_info):
    return struct(
        name = npm_info.package_name,
        label = npm_hub_label(npm_info),
        tree = npm_info.store.tree,
    )

def source_path(file):
    """A File's path when it is in the source tree, else None: the program
    root links these one by one and the output tree whole."""
    return file.path if file.is_source else None

def compiler_sources(args, srcs, chain, dep_dts, generated_srcs):
    args.add_all(
        depset(srcs + chain, transitive = [dep_dts]),
        map_each = source_path,
        format_each = "-source=%s",
    )
    args.add_all(generated_srcs, format_each = "-source=%s")

def ownership_manifest(ctx, own, direct, owners, npm_declared, npm_reachable):
    manifest = ctx.actions.declare_file("{}.ownership".format(ctx.label.name))
    lines = ctx.actions.args()
    lines.set_param_file_format("multiline")
    lines.add("label\t" + label_text(ctx.label))
    lines.add_all(own, format_each = "own\t%s")
    lines.add_all(direct, format_each = "direct\t%s")
    for record in owners.to_list():
        lines.add_all(
            record.files,
            format_each = "file\t{}\t%s".format(record.label),
            expand_directories = False,
        )
    lines.add_all([
        "npm-direct\t{}\t{}".format(link.name, link.tree.path)
        for link in npm_declared
    ])
    lines.add_all([
        "npm\t{}\t{}\t{}".format(package.name, package.label, package.tree.path)
        for package in npm_reachable
    ])
    ctx.actions.write(output = manifest, content = lines)
    return manifest

def program_args(
        ctx,
        root,
        srcs,
        chain,
        dep_dts,
        importers,
        overlays,
        manifests,
        generated_srcs = [],
        inherited_importers = []):
    args = ctx.actions.args()
    args.use_param_file("@%s", use_always = False)
    args.set_param_file_format("multiline")
    args.add("-root={}".format(root))
    compiler_sources(args, srcs, chain, dep_dts, generated_srcs)
    args.add_all(importers, format_each = "-node_modules=%s")
    args.add_all(inherited_importers, format_each = "-inherit_node_modules=%s")
    add_overlays(args, overlays)
    args.add_all(manifests, format_each = "-manifest=%s")
    return args

def add_overlays(args, overlays):
    for values, map_each in overlays:
        args.add_all(values, map_each = map_each, format_each = "-overlay=%s")

def program_inputs(tsconfig, srcs, chain, dep_dts, npm_files, extra = [], tool_files = depset()):
    return depset(
        srcs + ([tsconfig] if tsconfig else []) + chain + extra,
        transitive = [dep_dts, npm_files, tool_files],
    )

def _run_tsgo(ctx, run_args, inputs, outputs, mnemonic, checkers):
    requirements = {}
    if checkers > 0:
        run_args.add("--checkers", str(checkers))
        requirements["cpu:{}".format(checkers)] = ""
    ctx.actions.run(
        inputs = inputs,
        outputs = outputs,
        executable = get_tools_toolchain(ctx).tsaction,
        arguments = ["tsgo", run_args],
        mnemonic = mnemonic,
        progress_message = mnemonic + " %{label}",
        execution_requirements = requirements,
    )

def tsgo_check(
        ctx,
        tsgo,
        tsconfig,
        importers,
        overlays,
        manifests,
        srcs,
        chain,
        dep_dts,
        npm_files,
        ownership,
        checkers,
        runtime_scopes = [],
        generated_srcs = [],
        inherited_importers = []):
    """Registers TsgoCheck, the validation every program runs.

    `srcs`, `chain` and `dep_dts` are the program root's `-source` entries,
    each source-tree file linked at its own path while the output tree is
    linked whole; `importers` are the chain's node_modules directories
    nearest first, `inherited_importers` those deps and sources bring from
    outside it, `overlays` the output directories of the first-party deps
    at or above the target's package, `manifests` those deps' package.json as
    built, each laid at its package's path, `npm_files` the store files the
    program reaches and `ownership` the manifest the listing is checked
    against. Returns the stamp written when tsgo and the check pass, for the
    _validation group.
    """
    stamp = ctx.actions.declare_file("{}.tscheck".format(ctx.label.name))
    run_args = program_args(
        ctx,
        "{}/{}.program".format(tsconfig.dirname, ctx.label.name),
        srcs,
        chain,
        dep_dts,
        importers,
        overlays,
        manifests,
        generated_srcs,
        inherited_importers,
    )
    scope_inputs = runtime_scope_inputs(run_args, runtime_scopes)
    run_args.add(ownership, format = "-check=%s")
    if ctx.file.tsconfig:
        run_args.add(ctx.file.tsconfig, format = "-tsconfig=%s")
    run_args.add(stamp, format = "-stamp=%s")
    run_args.add("--")
    run_args.add(tsgo.tsgo_binary)
    run_args.add("--project", tsconfig)
    run_args.add("--noEmit")
    run_args.add("--explainFiles")
    run_args.add("--pretty", "false")
    _run_tsgo(
        ctx,
        run_args,
        program_inputs(
            tsconfig,
            srcs,
            chain,
            dep_dts,
            npm_files,
            [ownership] + scope_inputs,
            tool_files = tsgo.files,
        ),
        [stamp],
        "TsgoCheck",
        checkers,
    )
    return stamp
