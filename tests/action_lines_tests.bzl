"""Pins selected argv entries, written lines and launcher config sections of a target's actions."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_actions")

def _action_lines_impl(ctx):
    env = analysistest.begin(ctx)
    prefixes = tuple(ctx.attr.prefixes)
    lines = []
    for action in runnable_actions(env):
        outputs = [file.short_path for file in action.outputs.to_list()]
        if ctx.attr.output_suffix and not any([path.endswith(ctx.attr.output_suffix) for path in outputs]):
            continue
        label = action.mnemonic + " " + outputs[0] if outputs else action.mnemonic
        content = action.content if action.mnemonic == "FileWrite" else None
        if content != None and ctx.attr.json_keys:
            config = json.decode(content)
            section = config.get(config.get("mode"), config) if type(config) == "dict" else {}
            if type(section) == "dict" and any([key in section for key in ctx.attr.json_keys]):
                lines.extend([label + " " + key + "=" + json.encode(section.get(key)) for key in ctx.attr.json_keys])
        elif content != None:
            for line in content.split("\n"):
                if prefixes and line.startswith(prefixes):
                    lines.extend([label + " " + part for part in (line.split(ctx.attr.separator) if ctx.attr.separator else [line])])
        elif prefixes:
            lines.extend([label + " " + arg for arg in (action.argv or []) if arg.startswith(prefixes)])
    asserts.equals(env, ctx.attr.expected, sorted([line.replace(ctx.bin_dir.path, "$(BINDIR)") for line in lines]))
    return analysistest.end(env)

action_lines_test = analysistest.make(
    _action_lines_impl,
    attrs = {
        "expected": attr.string_list(),
        "json_keys": attr.string_list(),
        "output_suffix": attr.string(),
        "prefixes": attr.string_list(),
        "separator": attr.string(),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)
