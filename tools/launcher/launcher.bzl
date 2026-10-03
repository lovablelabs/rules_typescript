"""Wiring for the Go launcher the launcher toolchain resolves to.

An executable rule writes one JSON config and points its executable at a
symlink of the launcher.  Nothing generates shell text, so there is no quoting
layer to get wrong. The runfiles library locates the config and runtime tool;
native application inputs occupy the build-owned view beside the config.
"""

load("//ts/private:providers.bzl", "TsInfo", "canonical_runtime_file", "runtime_links")
load(
    "//ts/private:toolchain.bzl",
    "LAUNCHER_TOOLCHAIN_TYPE",
    "TSGO_PLATFORMS",
    "get_launcher_toolchain",
    "get_tools_toolchain",
)

LAUNCHER_TOOLCHAINS = [
    config_common.toolchain_type(LAUNCHER_TOOLCHAIN_TYPE, mandatory = False),
]

NativeExecutableInfo = provider(fields = ["launcher", "default_files"])

NATIVE_PROGRAM_ATTRS = {
    "public_name": attr.string(mandatory = True),
}

NATIVE_EXECUTABLE_ATTRS = {
    "native_program": attr.label(providers = [NativeExecutableInfo], mandatory = True),
}

def declare_runnable(name, producer, executable, kwargs, test = False):
    program_kwargs = {key: value for key, value in kwargs.items() if key not in ["size", "timeout", "flaky", "shard_count", "local"]}
    program_kwargs["visibility"] = ["//visibility:private"]
    if test:
        program_kwargs["testonly"] = True
    producer(name = name + "__native_program", public_name = name, **program_kwargs)
    executable(name = name, native_program = ":" + name + "__native_program", **kwargs)

def complete_native_executable(ctx, basename):
    program = ctx.attr.native_program
    info = program[NativeExecutableInfo]
    launcher = info.launcher
    executable = ctx.actions.declare_file(basename)
    config = ctx.actions.declare_file(basename + ".json")
    ctx.actions.symlink(output = executable, target_file = launcher.executable, is_executable = True)
    ctx.actions.symlink(output = config, target_file = launcher.config)
    files = [executable, config]
    mapping = program[DefaultInfo].files_to_run.repo_mapping_manifest
    if launcher.native_view and mapping:
        output = ctx.actions.declare_file(launcher.native_view + "/_repo_mapping")
        ctx.actions.run(
            executable = get_tools_toolchain(ctx).tsaction,
            arguments = ["stage", "-out=" + output.dirname, mapping.path, output.basename],
            inputs = [mapping],
            outputs = [output],
            mnemonic = "TsRunfilesMapping",
        )
        files.append(output)
    providers = [DefaultInfo(
        executable = executable,
        files = info.default_files,
        runfiles = program[DefaultInfo].default_runfiles.merge(ctx.runfiles(files = files, root_symlinks = {executable.basename + ".json": config})),
    )]
    for provider_type in [TsInfo, OutputGroupInfo]:
        if provider_type in program:
            providers.append(program[provider_type])
    return providers

def runfiles_link_path(file):
    return "external/" + file.short_path[3:] if file.short_path.startswith("../") else file.short_path

def runfiles_root_path(ctx, path):
    return path[len("external/"):] if path.startswith("external/") else ctx.workspace_name + "/" + path

def rlocation_path(ctx, file):
    return runfiles_root_path(ctx, runfiles_link_path(file))

def _runfiles_paths(ctx, runfiles):
    # Bazel's precedence is defined by Runfiles.getRunfilesInputs: https://github.com/bazelbuild/bazel/blob/9.2.0/src/main/java/com/google/devtools/build/lib/analysis/Runfiles.java
    paths = {link.path: link.target_file for link in runfiles.symlinks.to_list()}
    paths.update({runfiles_link_path(file): file for file in runfiles.files.to_list()})
    visible = {}
    for path, file in paths.items():
        parts = path.split("/")
        if not any(["/".join(parts[:depth]) in paths for depth in range(1, len(parts))]):
            visible[runfiles_root_path(ctx, path)] = file
    visible.update({runfiles_root_path(ctx, path): None for path in runfiles.empty_filenames.to_list()})
    visible.update({link.path: link.target_file for link in runfiles.root_symlinks.to_list()})
    return visible

def runfiles_scope_paths(ctx, runfiles, module_paths = []):
    demanded = {}
    for path in module_paths:
        parts = path.split("/")
        demanded.update({"/".join(parts[:depth]): True for depth in range(1, len(parts) + 1)})
    return {
        path: file
        for path, file in _runfiles_paths(ctx, runfiles).items()
        if path in demanded or path.endswith("/package.json") or (file != None and (file.basename == "package.json" or file.is_directory or file.is_symlink))
    }

def _available_destination(paths, destination):
    parts = destination.split("/")
    return not any(["/".join(parts[:depth]) in paths for depth in range(1, len(parts) + 1)]) and not any([path.startswith(destination + "/") for path in paths])

def _declare_native_view(ctx, base, config, runfiles, canonical_links, runtime_file):
    if runtime_file:
        runfiles = runfiles.merge(ctx.runfiles(files = [runtime_file]))

    # Bazel inserts its own mapping after advertised runfiles; completion supplies that exact File.
    paths = {path: file for path, file in _runfiles_paths(ctx, runfiles).items() if path != "_repo_mapping" and not path.startswith("_repo_mapping/")}
    modules = config.get("runtime_modules", [])
    links = runtime_links([struct(canonical_links = canonical_links)])
    selected = {}
    for path in modules:
        file = paths.get(path)
        if file == None or file.is_directory:
            fail("{}: native module '{}' has no individual File. Did you mean to publish its runtime File?".format(ctx.label, path))
        selected.setdefault(canonical_runtime_file(file, links), []).append(path)
    files = {file: True for file in paths.values() if file != None}
    canonical = {
        link: canonical_runtime_file(link, links)
        for link in links
        if link in files
    }
    files.update({target: True for target in canonical.values()})
    prefix = base + ".runtime"
    spec = ctx.actions.declare_file(prefix + ".json")
    root = spec.path[:-len(".json")] + "/view"
    outputs = []
    authorities = {}
    inputs = []
    by_path = {file.path: file for file in files}
    for file_path in sorted(by_path):
        file = by_path[file_path]
        path = prefix + "/files/" + file.path
        target = canonical.get(file)
        choices = selected.get(target or file, [])
        if len(choices) > 1:
            fail("{}: native File '{}' reaches multiple canonical module paths {}. Did you mean to expose one canonical module path?".format(ctx.label, file.path, choices))
        if target != None or choices:
            output = ctx.actions.declare_symlink(path)
            kind = "alias"
        elif file.is_symlink:
            output = ctx.actions.declare_symlink(path)
            kind = "symlink"
        elif file.is_directory:
            output = ctx.actions.declare_directory(path)
            kind = "directory"
        else:
            output = ctx.actions.declare_file(path)
            kind = "file"
        authorities[file] = output
        outputs.append(output)
        inputs.append({"path": file.path, "output": output.path, "kind": kind})
    for input in inputs:
        if input["kind"] != "alias":
            continue
        file = by_path[input["path"]]
        target = canonical.get(file, file)
        choices = selected.get(target, [])
        input["target"] = root + "/" + choices[0] if choices else authorities[target].path
    for path, file in paths.items():
        parts = path.split("/")
        if any(["/".join(parts[:depth]) in paths for depth in range(1, len(parts))]):
            continue
        output = ctx.actions.declare_file(prefix + "/view/" + path) if path in modules or file == None else ctx.actions.declare_symlink(prefix + "/view/" + path)
        outputs.append(output)
    section = config[config["mode"]]
    node_modules = section.get("node_modules", [])
    if type(node_modules) == "string":
        node_modules = [node_modules] if node_modules else []
    links = {}
    if node_modules:
        destination = config["workspace"] + "/node_modules"
        if _available_destination(paths, destination):
            links[destination] = node_modules[-1]
            outputs.append(ctx.actions.declare_symlink(prefix + "/view/" + destination))
    occupied = paths | {path: None for path in links}
    contexts = section.get("npm_contexts", [])
    for context in contexts:
        lookups = [(context["module"], context["bindings"])]
        scope = context.get("package_scope")
        if scope:
            lookups.append((scope["manifest"], scope["bindings"]))
        for anchor, bindings in lookups:
            directory = anchor.rsplit("/", 1)[0]
            for name in bindings:
                destination = directory + "/node_modules/" + name
                if _available_destination(occupied, destination):
                    outputs.append(ctx.actions.declare_symlink(prefix + "/view/" + destination))
                    occupied[destination] = None
    optional = section.get("optional_deps", [])
    for dep in optional:
        outputs.append(ctx.actions.declare_symlink(prefix + "/optional/node_modules/" + dep["name"]))
    ctx.actions.write(spec, json.encode({
        "root": root,
        "inputs": inputs,
        "entries": {path: file.path if file != None else "" for path, file in paths.items()},
        "modules": modules,
        "npm_contexts": contexts,
        "links": links,
        "optional_deps": optional,
    }))
    ctx.actions.run(
        executable = get_tools_toolchain(ctx).tsaction,
        arguments = ["native-view", "-spec=" + spec.path],
        inputs = depset([spec] + files.keys()),
        outputs = outputs,
        mnemonic = "TsNativeView",
        progress_message = "Preparing native runtime view for %{label}",
    )
    return outputs

def validate_runfiles_modules(ctx, visible, demanded, consumer):
    for path, expected in demanded:
        parts = path.split("/")
        for depth in range(len(parts) - 1, 0, -1):
            parent = "/".join(parts[:depth])
            if parent in visible:
                fail(("{} {}: cannot establish runtime File '{}' at '{}' beneath opaque runtime entry '{}'. " +
                      "Did you mean to keep that runfiles entry outside the module path, or publish individual Files?").format(
                    consumer,
                    ctx.label,
                    expected.short_path,
                    path,
                    parent,
                ))
        actual = visible.get(path)

        # Two targets sharing one link action yield distinct File objects for the same artifact.
        if actual != expected and (actual == None or actual.path != expected.path):
            fail(("{} {}: runtime path '{}' selects {} instead of published runtime File '{}' from {}. " +
                  "Did you mean to remove or relocate the conflicting data or runfiles entry?").format(
                consumer,
                ctx.label,
                path,
                "'{}' from {}".format(actual.short_path, actual.owner) if actual != None else "an empty or absent entry",
                expected.short_path,
                expected.owner,
            ))

def declare_launcher(ctx, config, basename = None, runfiles = None, canonical_links = [], runtime_file = None):
    """Writes a launcher config and the launcher symlink that reads it.

    The rule declares LAUNCHER_TOOLCHAINS and `fragments = ["platform"]`; a
    target platform no launcher toolchain covers fails here naming it.

    Args:
        ctx: the rule context.
        config: the config dict, serialised as the launcher's JSON contract.
        basename: name of the executable; defaults to "<target>_launcher".
        runfiles: admitted application inputs for a native runtime view.
        canonical_links: exact dependency alias and selected module File pairs.
        runtime_file: configured executable admitted to native runfiles lookup.

    Returns:
        A struct with the `executable` File, its `config` File, the `files`
        that must reach runfiles, and the `root_symlinks` dict that stages the
        config where the launcher can find it however it was started.
    """
    toolchain = get_launcher_toolchain(ctx)
    if toolchain == None:
        fail(("{}: no launcher toolchain resolved for the target platform " +
              "{}; rules_typescript ships the launcher for {} " +
              "(COMPATIBILITY.md#platforms).").format(
            ctx.label,
            ctx.fragments.platform.platform,
            ", ".join(TSGO_PLATFORMS),
        ))
    launcher = toolchain.launcher
    public_name = getattr(ctx.attr, "public_name", "")
    if public_name:
        config = dict(config)
        config["label"] = str(ctx.label.same_package_label(public_name))
    base = basename if basename else "{}_launcher".format(ctx.label.name)
    executable = ctx.actions.declare_file(base)
    config_file = ctx.actions.declare_file(base + ".json")
    native_files = []
    if config["mode"] in ["node", "node_test"]:
        native_files = _declare_native_view(ctx, base, config, runfiles, canonical_links, runtime_file)
        config = dict(config)
        config.pop("runtime_modules", None)
        section = dict(config[config["mode"]])
        section.pop("npm_contexts", None)
        config[config["mode"]] = section
        config["native_view_anchor"] = rlocation_path(ctx, config_file)

    ctx.actions.write(
        output = config_file,
        content = json.encode_indent(config, indent = "  "),
    )

    # One launcher binary for every target; the config beside the symlink is
    # per-target.
    ctx.actions.symlink(
        output = executable,
        target_file = launcher,
        is_executable = True,
    )

    # At the runfiles root, under the launcher's own basename: the one place
    # reachable from `bazel run`, `bazel test`, and from another rule's action
    # (where argv[0] is an exec path with nothing beside it).
    return struct(
        executable = executable,
        config = config_file,
        native_view = base + ".runtime/view" if config["mode"] in ["node", "node_test"] else "",
        files = [executable, config_file, launcher] + native_files,
        root_symlinks = {base + ".json": config_file},
    )
