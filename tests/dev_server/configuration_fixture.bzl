def _configuration_source_impl(ctx):
    source = ctx.actions.declare_file("configuration/value.ts")
    ctx.actions.write(source, "export const configuration: string = " + json.encode(source.path) + ";\n")
    return [DefaultInfo(files = depset([source]))]

configuration_source = rule(implementation = _configuration_source_impl)

def _transitioned_source_impl(ctx):
    producer = ctx.file.producer
    server = ctx.file.server
    layout = ctx.actions.declare_file(ctx.label.name + ".json")
    twin = ctx.actions.declare_file(ctx.label.name + ".server.ts")
    ctx.actions.symlink(output = twin, target_file = server)
    ctx.actions.write(layout, json.encode({
        "logical": producer.short_path,
        "producer": producer.path,
        "server": server.path,
        "server_bin": ctx.bin_dir.path,
    }))
    return [
        DefaultInfo(files = depset([producer])),
        OutputGroupInfo(fixture = depset([layout, twin])),
    ]

transitioned_source = rule(
    implementation = _transitioned_source_impl,
    attrs = {
        "producer": attr.label(allow_single_file = [".ts"], cfg = "exec", mandatory = True),
        "server": attr.label(allow_single_file = [".ts"], mandatory = True),
    },
)
