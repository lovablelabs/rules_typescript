"""The workspace-root tsconfig.json an IDE reads, built from the build graph.

Everything the file needs is already in the graph: a ts_compile target's package
is its source root, so the tsconfig is a declared output of a rule over an
aspect, and `bazel run //:refresh_tsconfig` only copies that output into the
source tree.

The `paths` map holds first-party packages: the package directory and its
bazel-bin twin, where a build leaves the .d.ts. npm packages are not in it. The
checkout's node_modules holds what the lockfile resolves, pnpm's links to the
workspace members included, and TypeScript walks it from the importing file the
way tsgo walks the chain a build lays out; a `paths` key naming a copy of a
package's declarations would only send the editor somewhere the build does not
look.
"""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load("//ts/private:providers.bzl", "TsConfigInfo")
load(
    "//ts/private:toolchain.bzl",
    "TOOLS_TOOLCHAIN_TYPE",
)

TsconfigSourcesInfo = provider(
    doc = "What a workspace-root tsconfig.json needs from the ts_compile targets under it.",
    fields = {
        "packages": "depset of struct(path, has_index): package of every ts_compile target reached, and whether it has an index file to name as the package entry point.",
        "option_groups": "depset of struct(package, label, options_json, extends, include): the program one target checks under, which the root block cannot carry -- the tsconfig it names, and allowJs for its JavaScript srcs. A target whose tsconfig turns `strict` off or names a `lib` is checked correctly by the build and wrongly by the editor unless the editor gets its own program for those files.",
    },
)

TsconfigFragmentInfo = provider(
    doc = "The compiler fragments an editor can merge without a rule ever naming the target.",
    fields = {
        "fragments": """depset of File: one fragment per compiler target that contributes a package.

Each is complete for its own closure -- the packages it reaches -- so any
one of them is a usable answer on its own, which is what makes a partially
built bazel-out still readable.""",
    },
)

WorkspaceCopyInfo = provider(
    doc = "Files a rule wants written into the source tree beside its default output.",
    fields = {
        "entries": "depset of struct(file, dest): each file and its workspace-relative destination.",
    },
)

def _has_index(target, ctx):
    package = target.label.package
    for src in getattr(ctx.rule.files, "srcs", []):
        for ext in [".ts", ".tsx", ".mts", ".js", ".mjs"]:
            if src.short_path == package + "/index" + ext:
                return True
    return False

FRAGMENT_SUFFIX = ".tsconfig-fragment.json"

_FRAGMENT_FORMAT = "tsconfig-fragment-v1"

def _fragment_package(package):
    return json.encode({"package": package.path, "index": package.has_index})

def _fragment(target, ctx, sources):
    """One JSON object per line, carrying `sources`, for the tsserver hook to merge.

    Deliberately not built from _packages: that materialises the depset, which
    is affordable once for one ide_tsconfig target and not on every target of
    every build. An Args with map_each defers the same work to execution time,
    so the depset stays a depset through analysis.
    """
    args = ctx.actions.args()
    args.set_param_file_format("multiline")
    args.add(json.encode({"format": _FRAGMENT_FORMAT, "label": str(target.label)}))
    args.add_all(sources.packages, map_each = _fragment_package, uniquify = True)

    out = ctx.actions.declare_file(target.label.name + FRAGMENT_SUFFIX)
    ctx.actions.write(out, args)
    return out

# The root block's own values.
_ROOT_TARGET = "ES2022"
_ROOT_JSX = "react-jsx"

# TypeScript compares these values case-insensitively, and treats `lib` as a set.
_CASE_INSENSITIVE_OPTIONS = [
    "target",
    "module",
    "moduleResolution",
    "jsx",
    "moduleDetection",
    "newLine",
]

def _canonical_options(options):
    canonical = dict(options)
    for key in _CASE_INSENSITIVE_OPTIONS:
        value = canonical.get(key)
        if type(value) == "string":
            canonical[key] = value.lower()
    lib = canonical.get("lib")
    if type(lib) == "list":
        canonical["lib"] = sorted([entry.lower() if type(entry) == "string" else entry for entry in lib])
    return canonical

def _option_group(target, ctx):
    """The editor program a target's srcs need beyond the root block, or []."""

    # A `manual` target is one nothing builds -- a fixture read by an analysis
    # test rather than run. There is no editor program to get right.
    if "manual" in getattr(ctx.rule.attr, "tags", []):
        return []

    # A .js source is only in the program at all with allowJs, which ts_compile
    # infers from srcs rather than making the author say it.
    sources = [f for src in ctx.rule.files.srcs for f in [src]]
    options = {}
    if any([f.extension in ("js", "jsx", "mjs", "cjs") for f in sources]):
        options["allowJs"] = True

    # The tsconfig this target checks against. An editor program for these files
    # has to start from the same place, or it disagrees for a second reason.
    extends = ""
    tsconfig = getattr(ctx.rule.attr, "tsconfig", None)
    if tsconfig and TsConfigInfo in tsconfig:
        extends = tsconfig[TsConfigInfo].tsconfig.short_path
    elif getattr(ctx.rule.file, "tsconfig", None):
        extends = ctx.rule.file.tsconfig.short_path

    package = target.label.package

    # The tsconfig.json in the target's own directory is already the program
    # tsserver reads for its files; the root only leaves them out.
    own = extends == (package + "/" if package else "") + "tsconfig.json"
    if own:
        options, extends = {}, ""
    if not options and not extends and not own:
        return []

    include = [
        f.short_path[len(package) + 1:] if package else f.short_path
        for f in sources
        if f.is_source and not f.short_path.endswith(".d.ts") and f.short_path.startswith(package)
    ]
    if not include:
        return []

    # JSON and a tuple, not a dict and a list: a depset element has to be
    # immutable, and these travel to _ide_tsconfig_impl through one.
    return [struct(
        package = package,
        label = str(target.label),
        options_json = json.encode(options),
        extends = extends,
        include = tuple(sorted(include)),
        own = own,
    )]

def _tsconfig_aspect_impl(target, ctx):
    # A link target reaches the view through `member`, the view the member
    # through `target`; ts_compile's own `target` is an ES version.
    reached = list(getattr(ctx.rule.attr, "deps", []))
    for attr in ("member", "target"):
        if type(getattr(ctx.rule.attr, attr, None)) == "Target":
            reached.append(getattr(ctx.rule.attr, attr))
    inherited = [dep[TsconfigSourcesInfo] for dep in reached if TsconfigSourcesInfo in dep]

    packages = []
    option_groups = []

    # A workspace member's view adds nothing of its own: the checkout's
    # node_modules holds pnpm's link to the member, and the editor resolves it there.
    if ctx.rule.kind in ("ts_compile", "ts_test"):
        if target.label.package:
            packages = [struct(
                path = target.label.package,
                has_index = _has_index(target, ctx),
            )]
        option_groups = _option_group(target, ctx)

    sources = TsconfigSourcesInfo(
        packages = depset(packages, transitive = [s.packages for s in inherited], order = "postorder"),
        option_groups = depset(option_groups, transitive = [s.option_groups for s in inherited], order = "postorder"),
    )

    fragments = [dep[TsconfigFragmentInfo].fragments for dep in reached if TsconfigFragmentInfo in dep]
    own = [_fragment(target, ctx, sources)] if packages else []
    fragments = depset(own, transitive = fragments, order = "postorder")

    return [
        sources,
        TsconfigFragmentInfo(fragments = fragments),
        OutputGroupInfo(ide_fragments = fragments),
    ]

tsconfig_aspect = aspect(
    implementation = _tsconfig_aspect_impl,
    attr_aspects = ["deps", "member", "target"],
    doc = """Collects the source roots an IDE tsconfig needs.

Also writes one `<target>.tsconfig-fragment.json` per compiler package owner, in the
`ide_fragments` output group. That group is how the tsserver hook gets the
targets no rule can name: an aspect propagates along dependency edges that
already exist and creates none, so it needs no visibility where
`ide_tsconfig(deps = [...])` needs a grant. Enable it in .bazelrc, where any
`bazel build` then refreshes the fragments as a side effect:

    build --aspects=@rules_typescript//ts/private:tsconfig_aspect.bzl%tsconfig_aspect
    build --output_groups=+ide_fragments
""",
)

_HEADER = "Generated by 'bazel run //:refresh_tsconfig'. Do not edit manually — re-run to update."

# tsserver resolves a plugin as <probe location>/node_modules/<plugin name>, so
# `.bazel` is the probe location an editor names and this is the package.
_PLUGIN_NAME = "@rules_typescript/tsserver-plugin"
_PLUGIN_DIR = ".bazel/node_modules/" + _PLUGIN_NAME

def _collect(sources, field):
    # The one place the graph has to become a file, so the one place a depset is
    # materialised.
    return depset(transitive = [getattr(s, field) for s in sources]).to_list()

def _packages(sources):
    """Every package the aspect reached, and whether any target in it has an index file."""
    indexed = {}
    for package in _collect(sources, "packages"):
        indexed[package.path] = indexed.get(package.path, False) or package.has_index
    return indexed

_ONE_ANSWER_PER_DIRECTORY = (
    "An editor resolves a file to a program by directory, so one\n" +
    "directory cannot hold both answers -- whichever were written, the\n" +
    "other target's sources would be checked against the wrong one.\n" +
    "Move one of them into its own package."
)

def _nested_configs(sources, root_options):
    """One editor program per package the root block cannot carry; none for a
    package whose targets check under its own tsconfig.json, tsserver's nearest.

    tsserver picks the nearest tsconfig.json walking up from a file, so a package
    whose targets check under a tsconfig of their own needs a file there that
    extends it. Grouping is by package because that is the only granularity
    tsserver has: two targets in one directory naming two tsconfigs have no
    representation at all, and that is an error rather than a silent pick --
    `extends` would take both, but TypeScript applies the array later-wins, so
    one file's keys would replace the other's for both targets' sources.

    allowJs is dropped where the root already sets it, so a target that only
    restates the defaults produces no file. That subtraction is sound only
    without a tsconfig: the root is the FIRST entry of the nested `extends`
    array, so a tsconfig after it wins every key the group leaves out.
    """
    root_options = _canonical_options(root_options)

    groups = {}
    own = {}
    for entry in sources.option_groups.to_list():
        group = groups.setdefault(entry.package, struct(
            options = {},
            extends = {},
            include = {},
        ))
        if entry.own:
            own[entry.package] = True
        group.options.update(json.decode(entry.options_json))
        if entry.extends:
            for baseline, owner in group.extends.items():
                if baseline != entry.extends:
                    fail(
                        "ts_refresh_tsconfig: {} and {} are in the same package and\n".format(
                            owner,
                            entry.label,
                        ) +
                        "  extend the tsconfig baselines {} and {}.\n".format(
                            json.encode(baseline),
                            json.encode(entry.extends),
                        ) +
                        _ONE_ANSWER_PER_DIRECTORY,
                    )
            group.extends[entry.extends] = entry.label
        for path in entry.include:
            group.include[path] = True

    out = []
    for package in sorted(groups):
        group = groups[package]
        if package in own:
            out.append(struct(
                package = package,
                own = True,
                options = {},
                extends = [],
                include = sorted(group.include),
            ))
            continue
        options = group.options
        if not group.extends:
            options = {
                key: value
                for key, value in options.items()
                if key not in root_options or root_options[key] != value
            }
        if not options and not group.extends:
            continue
        out.append(struct(
            package = package,
            own = False,
            options = options,
            extends = sorted(group.extends),
            include = sorted(group.include),
        ))
    return out

def _nested_config_json(package, group, tsconfig_path):
    """The nested tsconfig's own content.

    `extends` is an array with the root FIRST so that a package baseline the
    targets already check against wins over it. `include`/`exclude` are written
    rather than inherited: a relative path in an extended config is re-resolved
    against the extending file, so an inherited root `exclude` would name
    something else entirely here. Inherited `paths` are not re-resolved, which is
    why the root's package keys still work from down here.
    """
    depth = len(package.split("/"))
    to_workspace = "/".join([".."] * depth) + "/"
    to_root = to_workspace + tsconfig_path
    extends = [to_root] + [_relative_to(package, path, to_workspace) for path in group.extends]

    # An editor program must not write anything, and it is the one place these can
    # be pinned: they sit in the nested file's OWN compilerOptions, which beat
    # every `extends`. A package baseline that sets outDir/incremental -- as
    # //tests/compiler_options/baseline's does -- otherwise wins over the root's
    # noEmit and leaves emitted files and a .tsbuildinfo in the source tree. The
    # whole group has to go together: composite implies incremental, so turning
    # only the latter off is TS6379.
    options = dict(group.options)
    options["noEmit"] = True
    options["composite"] = False
    options["incremental"] = False

    # rootDir too, and to this package. A baseline is a Bazel tsconfig, so its
    # rootDir is whatever directory oxc strips for the target that uses it, and a
    # program covering the whole package is not under that -- //vite's baseline
    # says vite/src, which puts vite/tsup.config.ts outside it (TS6059).
    options["rootDir"] = "."

    return {
        "_comment": _HEADER,
        "extends": extends,
        # `files` is restated: a baseline's is a Bazel one, naming what Bazel
        # passes rather than what exists (tests/compiler_options/baseline, TS6053).
        "files": [],
        "compilerOptions": options,
        "include": group.include,
        "exclude": [],
    }

def _relative_to(package, path, to_workspace):
    """`path`, which is workspace-relative, expressed from inside `package`."""
    prefix = package + "/"
    if path.startswith(prefix):
        return "./" + path[len(prefix):]
    return to_workspace + path

def _ide_tsconfig_impl(ctx):
    sources = [dep[TsconfigSourcesInfo] for dep in ctx.attr.deps]
    bin_dir = "./{}bin".format(ctx.attr.symlink_prefix)

    packages = _packages(sources)

    paths = {}
    for package in sorted(packages):
        key = ("@/" + package.removeprefix("src/")) if package.startswith("src/") else package

        # Only a package that has one can name an entry point; the rest are
        # reachable through the wildcard alone.
        if packages[package]:
            paths[key] = ["./{}/index".format(package)]
        paths[key + "/*"] = ["./{}/*".format(package), "{}/{}/*".format(bin_dir, package)]

    config = {
        "_comment": _HEADER,
        "compilerOptions": {
            "strict": True,
            "target": _ROOT_TARGET,
            "module": "Preserve",
            "moduleResolution": "Bundler",
            "jsx": _ROOT_JSX,
            "declaration": True,
            "sourceMap": True,
            "skipLibCheck": True,
            "esModuleInterop": True,
            "rootDirs": [".", bin_dir],
            "paths": paths,
            "noEmit": True,
            # Not sufficient on its own: tsserver resolves a plugin named here
            # against its probe locations, so a client that passes no
            # --pluginProbeLocations still loads nothing. It is here so the
            # entry survives the next refresh, which rewrites this file whole.
            "plugins": [{"name": _PLUGIN_NAME}],
        },
        "exclude": [
            "**/bazel-*",
            "**/node_modules",
            "**/dist",
            "**/build",
            "**/.next",
            "**/.nuxt",
            ".bazel",
        ] + ctx.attr.extra_exclude,
    }

    # A package whose targets disagree with the block above gets its own program.
    # Its files leave the root one FILE BY FILE rather than by directory: a file
    # no ts_compile claims is still worth checking, and excluding the directory
    # would drop it from the editor entirely.
    nested = _nested_configs_for(ctx, config["compilerOptions"])
    nested_outs = []
    for group in nested:
        config["exclude"] = config["exclude"] + [
            "{}/{}".format(group.package, path)
            for path in group.include
        ]
        if group.own:
            continue
        nested_out = ctx.actions.declare_file("{}.nested.{}.json".format(
            ctx.label.name,
            group.package.replace("/", "_"),
        ))
        ctx.actions.write(
            nested_out,
            json.indent(
                json.encode(_nested_config_json(
                    group.package,
                    group,
                    ctx.attr.tsconfig_path,
                )),
                indent = "  ",
            ) + "\n",
        )
        nested_outs.append(struct(
            file = nested_out,
            dest = group.package + "/tsconfig.json",
        ))

    _check_declared_nested(ctx, [n.dest for n in nested_outs])

    out = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(out, json.indent(json.encode(config), indent = "  ") + "\n")
    return [
        DefaultInfo(files = depset([out])),
        WorkspaceCopyInfo(entries = depset(nested_outs)),
        OutputGroupInfo(nested_tsconfigs = depset([n.file for n in nested_outs])),
    ]

def _nested_configs_for(ctx, root_options):
    sources = [dep[TsconfigSourcesInfo] for dep in ctx.attr.deps]
    merged = struct(option_groups = depset(transitive = [s.option_groups for s in sources]))
    return _nested_configs(merged, root_options)

def _check_declared_nested(ctx, computed):
    """Fails when the checked-in set of nested configs is not the computed one.

    glob() cannot see across package boundaries, so the macro cannot discover
    these on its own -- and an orphan left behind after a package's options
    converge with the root would silently own that whole subtree in the editor.
    So the set is declared, and disagreement in either direction is an error.
    """
    declared = sorted(ctx.attr.nested_tsconfigs)
    if declared == sorted(computed):
        return
    missing = [p for p in computed if p not in declared]
    extra = [p for p in declared if p not in computed]
    fail(
        "ts_refresh_tsconfig: the nested_tsconfigs list does not match what the\n" +
        "graph needs.\n" +
        ("  add:    {}\n".format(", ".join(missing)) if missing else "") +
        ("  remove: {}\n".format(", ".join(extra)) if extra else "") +
        "Each entry is a package that needs its own editor program because its\n" +
        "targets' compilerOptions disagree with the root block. The list is\n" +
        "declared rather than discovered because glob() cannot cross a package\n" +
        "boundary, and an entry left behind would own its subtree in the editor.",
    )

_IDE_ATTRS = {
    "deps": attr.label_list(
        aspects = [tsconfig_aspect],
        doc = """The ts_compile and ts_test targets the IDE should see.

The aspect walks `deps` from here, so a target whose sources another listed
target already depends on does not need its own entry. A ts_test is a test
target, so the targets the macro declares over this list are testonly.""",
    ),
}

ide_tsconfig = rule(
    implementation = _ide_tsconfig_impl,
    attrs = dict(
        _IDE_ATTRS,
        nested_tsconfigs = attr.string_list(
            doc = "Every package that gets its own generated tsconfig.json, as a " +
                  "workspace-relative path to that file. Declared rather than " +
                  "discovered, and checked against what the graph needs.",
        ),
        tsconfig_path = attr.string(
            default = "tsconfig.json",
            doc = "Where the root tsconfig is written, so a nested one can point " +
                  "`extends` back at it.",
        ),
        symlink_prefix = attr.string(
            default = "bazel-",
            doc = "Value of --symlink_prefix, which names the bazel-bin symlink the IDE reads .d.ts through.",
        ),
        extra_exclude = attr.string_list(
            doc = """Globs to add to the generated `exclude`, on top of the built-in ones.

`include` stays `**/*`, so a directory holding TypeScript that is not in this
module's build graph -- a nested Bazel module, listed in .bazelignore -- is in
the program until something excludes it. Nothing in the graph names such a
directory, so this is where a workspace says so. Anchor each glob with `**/`:
an unanchored one only matches at the workspace root.""",
        ),
    ),
    doc = """Writes a workspace-root tsconfig.json for IDE consumption.

The file is a declared output; it is Bazel's copy, not the workspace's. Pair it
with refresh_workspace_files to put it in the source tree, and with diff_test to
fail when the checked-in copy has gone stale.""",
)

def _ide_hook_data_impl(ctx):
    sources = [dep[TsconfigSourcesInfo] for dep in ctx.attr.deps]

    data = {
        "_comment": _HEADER,
        "packages": sorted(_packages(sources)),
    }

    out = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(out, json.indent(json.encode(data), indent = "  ") + "\n")
    return [DefaultInfo(files = depset([out]))]

ide_hook_data = rule(
    implementation = _ide_hook_data_impl,
    attrs = _IDE_ATTRS,
    doc = """Writes what the tsserver hook needs from the build graph.

The hook runs inside a long-lived editor process, so it reads this file rather
than asking Bazel: the package list is an analysis-time fact, and querying for
it would fight the server lock.""",
)

def _rlocation(ctx, file):
    if file.short_path.startswith("../"):
        return file.short_path[3:]
    return ctx.workspace_name + "/" + file.short_path

def _refresh_workspace_files_impl(ctx):
    entries = []
    inputs = []
    for src, dest in ctx.attr.files.items():
        files = src[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("refresh_workspace_files: {} produces {} files, want exactly one.".format(src.label, len(files)))
        inputs.append(files[0])
        entries.append({"rlocation": _rlocation(ctx, files[0]), "dest": dest})

        if WorkspaceCopyInfo not in src:
            continue
        for copy in src[WorkspaceCopyInfo].entries.to_list():
            inputs.append(copy.file)
            entries.append({"rlocation": _rlocation(ctx, copy.file), "dest": copy.dest})

    manifest = ctx.actions.declare_file(ctx.label.name + ".manifest.json")
    ctx.actions.write(manifest, json.encode(entries))

    tools = ctx.toolchains[TOOLS_TOOLCHAIN_TYPE]
    launcher = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = launcher,
        target_file = tools.tools_info.copy_to_workspace,
        is_executable = True,
    )

    return [
        DefaultInfo(
            executable = launcher,
            runfiles = ctx.runfiles(files = inputs + [manifest]).merge(
                tools.default.default_runfiles,
            ),
        ),
        RunEnvironmentInfo(environment = {"COPY_TO_WORKSPACE_MANIFEST": _rlocation(ctx, manifest)}),
    ]

refresh_workspace_files = rule(
    implementation = _refresh_workspace_files_impl,
    executable = True,
    attrs = {
        "files": attr.label_keyed_string_dict(
            doc = """Maps each single-file target to its destination, relative to the workspace root.

A target that also returns WorkspaceCopyInfo contributes the files named there,
each to the destination that provider carries.""",
            allow_empty = False,
            allow_files = True,
        ),
    },
    toolchains = [TOOLS_TOOLCHAIN_TYPE],
    doc = """Copies build outputs into the source tree under `bazel run`.

The copy is all it does: what to write is decided at analysis time, by the rules
that declared the outputs.""",
)

def _nested_tsconfig_file_impl(ctx):
    """One nested tsconfig out of the generator's output group, by destination.

    diff_test compares two single files, and the generator produces a set of
    them; without this each nested file would need its own rule instance in the
    generator, which the generator cannot know the names of at load time.
    """
    wanted = ctx.attr.dest.replace("/", "_")
    for entry in ctx.attr.generator[WorkspaceCopyInfo].entries.to_list():
        if entry.dest == ctx.attr.dest:
            return [DefaultInfo(files = depset([entry.file]))]
    fail("no generated tsconfig for {} (looked for {})".format(ctx.attr.dest, wanted))

_nested_tsconfig_file = rule(
    implementation = _nested_tsconfig_file_impl,
    attrs = {
        "generator": attr.label(
            doc = "The ide_tsconfig target whose nested outputs to pick from.",
            providers = [WorkspaceCopyInfo],
        ),
        "dest": attr.string(
            doc = "The workspace-relative destination naming which nested file to take.",
        ),
    },
)

def ts_refresh_tsconfig(
        name = "refresh_tsconfig",
        deps = [],
        tsconfig = "tsconfig.json",
        extra_exclude = [],
        nested_tsconfigs = [],
        test = False):
    """Declares the IDE tsconfig, the run target that installs it, and its staleness test.

    Args:
        name:     Name of the `bazel run` target. The generated files are
                  `<name>.generated` and `<name>.hook_data`, the diff test
                  `<name>_test`.
        deps:     ts_compile and ts_test targets the IDE should see. The
                  aspect follows `deps` from each one.
        tsconfig: Where in the workspace the file is written.
        extra_exclude:
                  Globs added to the generated `exclude`, for TypeScript trees
                  that are not in this module's build graph. Anchor each with
                  `**/`.
        nested_tsconfigs:
                  Packages that need their own editor program, as
                  workspace-relative paths to the tsconfig.json each one gets
                  (e.g. "tests/compiler_options/jsx/tsconfig.json"). A package
                  belongs here when its targets name a tsconfig of their own,
                  because an editor resolves a file to a program by directory
                  and the root program would check those files against the
                  wrong options. The list is declared rather
                  than discovered (glob() cannot cross a package boundary) and
                  the rule fails when it disagrees with the graph in either
                  direction.
        test:     Add a diff_test that fails when `tsconfig` is stale. Turn it
                  on once `tsconfig` is checked in.
    """
    ide_tsconfig(
        name = name + ".generated",
        testonly = True,
        deps = deps,
        extra_exclude = extra_exclude,
        nested_tsconfigs = nested_tsconfigs,
        tsconfig_path = tsconfig,
    )
    for nested in nested_tsconfigs:
        _nested_tsconfig_file(
            name = "{}.nested.{}".format(name, nested.replace("/", "_").replace(".", "_")),
            testonly = True,
            generator = ":" + name + ".generated",
            dest = nested,
        )
    ide_hook_data(
        name = name + ".hook_data",
        testonly = True,
        deps = deps,
    )
    refresh_workspace_files(
        name = name,
        testonly = True,
        files = {
            ":" + name + ".generated": tsconfig,
            ":" + name + ".hook_data": ".bazel/tsserver-hook-data.json",
            "@rules_typescript//tools:tsserver-hook.js": ".bazel/tsserver-hook.js",
            "@rules_typescript//tools:tsserver-hook-resolver.js": ".bazel/tsserver-hook-resolver.js",
            "@rules_typescript//tools:tsserver-hook-worker.js": ".bazel/tsserver-hook-worker.js",
            "@rules_typescript//tools:tsserver-plugin.js": _PLUGIN_DIR + "/index.js",
            "@rules_typescript//tools:tsserver-plugin-package.json": _PLUGIN_DIR + "/package.json",
            "@rules_typescript//tools:tsserver-plugin-resolver": _PLUGIN_DIR + "/tsserver-hook-resolver.js",
            "@rules_typescript//tools:tsserver-plugin-worker": _PLUGIN_DIR + "/tsserver-hook-worker.js",
        },
        visibility = ["//visibility:public"],
    )
    if test:
        diff_test(
            name = name + "_test",
            size = "small",
            failure_message = "{} is stale: run `bazel run //:{}`.".format(tsconfig, name),
            file1 = ":" + name + ".generated",
            file2 = tsconfig,
        )
        for nested in nested_tsconfigs:
            slug = nested.replace("/", "_").replace(".", "_")
            diff_test(
                name = "{}_nested_{}_test".format(name, slug),
                size = "small",
                failure_message = "{} is stale: run `bazel run //:{}`.".format(nested, name),
                file1 = ":{}.nested.{}".format(name, slug),
                file2 = "//{}:tsconfig.json".format(nested.rsplit("/", 1)[0]),
            )
