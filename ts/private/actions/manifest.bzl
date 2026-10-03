"""Writes workspace publication manifests or scopes projected from runtime File pairs."""

load("//ts/private:toolchain.bzl", "get_tools_toolchain")

def manifest_action(ctx, src, out, tsx_extension = None, runtime_targets = None, declaration_targets = None):
    args = ctx.actions.args()
    args.use_param_file("@%s", use_always = False)
    args.set_param_file_format("multiline")
    if tsx_extension != None:
        args.add("-tsx=" + tsx_extension)
    if runtime_targets != None:
        args.add("-runtime_targets=" + json.encode(runtime_targets))
    if declaration_targets != None:
        args.add("-declaration_targets=" + json.encode(declaration_targets))
    args.add(src)
    args.add(out)
    ctx.actions.run(
        inputs = [src],
        outputs = [out],
        executable = get_tools_toolchain(ctx).tsaction,
        arguments = ["manifest", args],
        mnemonic = "TsManifest",
        progress_message = "TsManifest %{label}",
    )

def runtime_scope_inputs(args, scopes):
    inputs = []
    for scope in scopes:
        args.add("-runtime_scope=" + json.encode({"source": scope.source.path, "runtime": scope.runtime.path, "targets": scope.targets}))
        inputs.extend([scope.source, scope.runtime])
    return inputs
