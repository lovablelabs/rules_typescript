"""Bind native model observations to real compiler and provider identities."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//tests:runnable_actions.bzl", "runnable_action_aspect", "runnable_action_label", "runnable_actions")
load("//tools/launcher:launcher.bzl", "rlocation_path", "runfiles_root_path")
load("//ts:defs.bzl", "TsInfo", "TsTestRunnerInfo", "ts_binary", "ts_codegen", "ts_compile", "ts_test")
load("//ts/private:providers.bzl", "canonical_runtime_file", "label_text", "require_runtime_scopes", "runtime_links", "runtime_mappings", "ts_info")

_Observation = provider(fields = ["info", "mappings", "live", "subject"])

def _fields(value):
    return {field: getattr(value, field) for field in dir(value) if field not in ["to_json", "to_proto"]}

# Analysis observations need individual Files to compare provider and publication identity.
def _runtime(info, source):
    candidates = {
        runtime: True
        for owner in info.owners.to_list()
        for original, runtime in getattr(owner, "runtime_files", ())
        if original == source
    }
    candidates.update({
        published: True
        for owner in info.owners.to_list()
        for original, _coordinate, published in getattr(owner, "asset_files", ())
        if original == source
    })
    if not candidates and all([not hasattr(owner, "runtime_files") for owner in info.owners.to_list()]):
        candidates.update({file: True for file in info.js.to_list()})
    if len(candidates) != 1:
        fail("producer fixture has no unique runtime File. Did you mean to bind one real producer output?")
    return candidates.keys()[0]

def _primary_impl(ctx):
    return [DefaultInfo(files = depset([_runtime(ctx.attr.producer[TsInfo], ctx.file.source)]))]

_primary = rule(
    implementation = _primary_impl,
    attrs = {
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _opaque_impl(ctx):
    facts = json.decode(ctx.attr.facts)
    runtime = ctx.actions.declare_file("/".join(facts["runtime_path"][len(facts["producer_package"]):]))
    declaration = ctx.actions.declare_file("entry.d.ts")
    if facts["runner"]:
        ctx.actions.expand_template(template = ctx.file.source, output = runtime, substitutions = {})
    else:
        ctx.actions.write(runtime, "export const value = 42;\n")
    ctx.actions.write(declaration, "export declare const value: number;\n")
    files = {"source": ctx.file.source, "runtime": runtime, "declaration": declaration}
    info = ts_info(js = depset([files[identity] for identity in facts["producer_js"]]), declarations = depset([declaration]), sources = depset([ctx.file.source]))
    fields = _fields(info)

    # 9aad440's emitted owner.files contains sources/declarations, but excludes emitted JavaScript.
    fields["owners"] = depset([struct(
        label = label_text(ctx.label),
        files = depset([files[identity] for identity in facts["producer_files"]]),
        declarations = depset([declaration]),
        type_inputs = depset([declaration]),
        importers = (),
    )])
    return [TsInfo(**fields), DefaultInfo(files = depset([runtime]))]

_opaque = rule(
    implementation = _opaque_impl,
    attrs = {
        "facts": attr.string(mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _facts_impl(ctx):
    info = ctx.attr.producer[TsInfo]
    runtime = _runtime(info, ctx.file.source)
    facts = json.decode(ctx.attr.facts)
    files = {"source": ctx.file.source, "other_source": ctx.file.other_source, "runtime": runtime}
    for identity in sorted({identity: True for pair in facts["aliases"] for identity in pair if identity != "runtime"}):
        files[identity] = ctx.actions.declare_file("links/" + identity + ".test.js")
    for identity, file in files.items():
        if identity not in ["source", "other_source", "runtime"]:
            ctx.actions.symlink(output = file, target_file = runtime)
    owners = info.owners.to_list()
    if len(facts["origins"]) > 1:
        changed = []
        for owner in owners:
            fields = _fields(owner)
            if owner.label == label_text(ctx.attr.producer.label):
                fields["runtime_files"] = tuple([(files[source], files[published]) for source, published, _owner in facts["origins"]])
            changed.append(struct(**fields))
        owners = changed
    fields = _fields(info)
    fields["owners"] = depset(owners + [struct(
        label = label_text(ctx.label),
        files = depset(),
        declarations = depset(),
        type_inputs = depset(),
        importers = (),
        runtime_files = (),
        canonical_links = tuple([(files[link], files[target]) for link, target in facts["aliases"]]),
    )])
    return [TsInfo(**fields), DefaultInfo(files = depset([files[facts["request"]]])), OutputGroupInfo(**{identity: depset([file]) for identity, file in files.items()})]

_facts = rule(
    implementation = _facts_impl,
    attrs = {
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
        "other_source": attr.label(allow_single_file = True, mandatory = True),
        "facts": attr.string(mandatory = True),
    },
)

def _alias_data_impl(ctx):
    request = ctx.attr.request
    fields = _fields(request[TsInfo])
    fields["transitive_data"] = depset(transitive = [fields["transitive_data"], request[DefaultInfo].files], order = "postorder")
    return [TsInfo(**fields), request[DefaultInfo]]

_alias_data = rule(
    implementation = _alias_data_impl,
    attrs = {"request": attr.label(providers = [TsInfo], mandatory = True)},
)

def _probe_impl(ctx):
    info = ctx.attr.subject[TsInfo]
    live = {file: True for file in depset(transitive = [info.transitive_js, info.transitive_data, info.transitive_runtime_sources]).to_list()}
    mappings = runtime_mappings(info.owners.to_list(), live)
    require_runtime_scopes(ctx.label, info, "producer publication conformance")
    return [_Observation(info = info, live = live, mappings = mappings, subject = ctx.attr.subject.label), DefaultInfo()]

_probe = rule(
    implementation = _probe_impl,
    attrs = {"subject": attr.label(providers = [TsInfo], mandatory = True)},
)

def _accepted_impl(ctx):
    env = analysistest.begin(ctx)
    case = json.decode(ctx.attr.case)
    facts = case["input"]
    expected = case["expected"]
    observation = analysistest.target_under_test(env)[_Observation]
    info = observation.info
    producer = ctx.attr.producer[TsInfo]
    source = ctx.file.source
    runtime = _runtime(producer, source)
    owners = info.owners.to_list()
    originals = {"producer": [owner for owner in producer.owners.to_list() if owner.label == label_text(ctx.attr.producer.label)]}
    if ctx.attr.peer:
        originals["peer"] = [owner for owner in ctx.attr.peer[TsInfo].owners.to_list() if owner.label == label_text(ctx.attr.peer.label)]
    ids = {"source": source, "runtime": runtime}
    asserts.equals(env, ids[facts["runtime_id"]], runtime, "source-mode runtime identity follows the native fact")
    labels = {"producer": label_text(ctx.attr.producer.label), "consumer": label_text(observation.subject)}
    if ctx.attr.peer:
        labels["peer"] = label_text(ctx.attr.peer.label)
    for identity in expected["owners"]:
        asserts.equals(env, originals[identity], [owner for owner in owners if owner.label == labels[identity]], "producer record survives unchanged")
    for identity in expected["live"]:
        asserts.true(env, ids[identity] in observation.live, "canonical File remains in the live closure")
    links = runtime_links(owners)
    actual_origins = {
        (original_source, published, owner.label): True
        for owner, pairs in observation.mappings
        for original_source, published in pairs
        if canonical_runtime_file(published, links) == runtime
    }
    wanted_origins = {(ids[original_source], ids[published], labels[owner]): True for original_source, published, owner in expected["origins"]}
    asserts.equals(env, wanted_origins, actual_origins, "runtime mappings match the native trace")
    assets = [triple for owner in owners for triple in getattr(owner, "asset_files", ())]
    relevant = {runtime: True}
    relevant.update({link: True for link in links if link in observation.live and canonical_runtime_file(link, links) == runtime})
    relevant.update({published: True for original_source, _coordinate, published in assets if original_source == source and published in observation.live})
    by_path = {}
    for file in relevant:
        by_path.setdefault(file.short_path, []).append(file)
    for view in expected["views"]:
        path = "/".join(view["path"])
        candidates = by_path.get(path, [])
        asserts.true(env, bool(candidates), "missing publication at " + path)
        if view["kind"] == "canonical":
            asserts.true(env, runtime in candidates, "canonical coordinate retains the exact runtime File")
        elif view["kind"] == "alias":
            asserts.true(env, any([canonical_runtime_file(file, links) == runtime for file in candidates]), "publication reaches the canonical File")
        elif view["kind"] == "asset":
            coordinate = "/".join(view["coordinate"])
            asserts.true(env, any([(source, coordinate, file) in assets for file in candidates]), "asset view retains its producer origin and coordinate")
            asserts.false(env, any([file in links for file in candidates]), "ordinary assets do not acquire module aliases")
    direct_data = {file: True for file in info.data.to_list()}
    asserts.equals(env, {"/".join(path): True for path in expected["direct_data"]}, {file.short_path: True for file in direct_data if file in relevant}, "direct data excludes inherited foreign canonical Files")
    asserts.equals(env, {"/".join(path): True for path in expected["direct_js"]}, {file.short_path: True for file in info.js.to_list() if file in relevant}, "direct JavaScript follows the requested source publication")
    for path in expected["copyable"]:
        asserts.true(env, any([file in direct_data and file not in links for file in by_path.get("/".join(path), [])]), "member-local ordinary asset remains copyable")
    if facts["kind"] == "opaque":
        ids["declaration"] = producer.declarations.to_list()[0]
        asserts.false(env, hasattr(originals["producer"][0], "runtime_files"), "prior wire shape omits runtime mappings")
        asserts.equals(env, {ids[identity]: True for identity in facts["producer_files"]}, {file: True for file in originals["producer"][0].files.to_list()}, "prior owner.files matches the native input facts")
        asserts.equals(env, {ids[identity]: True for identity in facts["producer_js"]}, {file: True for file in producer.transitive_js.to_list()}, "prior transitive_js matches the native input facts")
    return analysistest.end(env)

_accepted_test = analysistest.make(
    _accepted_impl,
    attrs = {
        "case": attr.string(mandatory = True),
        "peer": attr.label(providers = [TsInfo]),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _native_alias_impl(ctx):
    env = analysistest.begin(ctx)
    case = json.decode(ctx.attr.case)
    facts = case["input"]
    ids = {"runtime": _runtime(ctx.attr.producer[TsInfo], ctx.file.source)}
    ids.update({
        identity: getattr(ctx.attr.request[OutputGroupInfo], identity).to_list()[0]
        for identity in {identity: True for pair in facts["aliases"] for identity in pair}
        if identity != "runtime"
    })
    canonical = [ids[identity] for identity in case["expected"]["live"]]
    specs = [
        action
        for action in runnable_actions(env)
        if any([file.basename.endswith(".runtime.json") for file in action.outputs.to_list()])
    ]
    asserts.equals(env, 1, len(specs), "one native view binds the accepted alias facts")
    if specs:
        spec = json.decode(specs[0].content)
        inputs = {input["path"]: input for input in spec["inputs"]}
        asserts.equals(env, sorted([rlocation_path(ctx, file) for file in canonical]), spec["modules"], "native modules retain the canonical Files from the trace")
        requested = inputs[ids[facts["request"]].path]
        asserts.equals(env, "alias", requested["kind"], "the requested alias keeps its declared identity")
        asserts.equals(env, spec["root"] + "/" + rlocation_path(ctx, canonical[0]), requested["target"], "the native alias targets the trace's canonical File directly")
        for identity, file in ids.items():
            if identity != facts["request"] and identity not in case["expected"]["live"]:
                asserts.false(env, file.path in inputs, "alias-only intermediate Files need no native input: " + identity)
    return analysistest.end(env)

_native_alias_test = analysistest.make(
    _native_alias_impl,
    extra_target_under_test_aspects = [runnable_action_aspect],
    attrs = {
        "case": attr.string(mandatory = True),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "request": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _rejected_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, json.decode(ctx.attr.case)["expected"]["error"])
    return analysistest.end(env)

_rejected_test = analysistest.make(_rejected_impl, expect_failure = True, attrs = {"case": attr.string(mandatory = True)})

def _observed_runner_impl(ctx):
    original = ctx.attr.original[TsTestRunnerInfo]

    def launch(test_ctx, test):
        record = test_ctx.actions.declare_file(test_ctx.label.name + ".producer_roots.json")
        test_ctx.actions.write(record, json.encode({
            "entry_points": [file.path for file in test.entry_points],
            "canonical_links": [(link.path, canonical.path) for link, canonical in test.canonical_links],
            "runtime_files": [(source.path, runtime.path) for source, runtime in test.runtime_files],
            "runtime_inputs": [(source.path, runtime.path) for source, runtime in test.runtime_inputs.items()] if hasattr(test, "runtime_inputs") else None,
        }))
        return original.launch(test_ctx, test)

    fields = _fields(original)
    fields["launch"] = launch
    return [TsTestRunnerInfo(**fields)]

_observed_runner = rule(
    implementation = _observed_runner_impl,
    attrs = {"original": attr.label(providers = [TsTestRunnerInfo], mandatory = True)},
)

def _written_action(env, suffix):
    found = [
        action
        for action in runnable_actions(env)
        if action.mnemonic == "FileWrite"
        for output in action.outputs.to_list()
        if output.short_path.endswith(suffix)
    ]
    asserts.equals(env, 1, len(found), "actual runner action " + suffix)
    return found[0] if len(found) == 1 else None

def _runner_alias_impl(ctx):
    env = analysistest.begin(ctx)
    case = json.decode(ctx.attr.case)
    ids = {"runtime": _runtime(ctx.attr.producer[TsInfo], ctx.file.source)}
    record = _written_action(env, ".producer_roots.json")
    if record:
        actual = dict(json.decode(record.content)["canonical_links"])
        asserts.equals(env, [ids[identity].path for identity in case["expected"]["live"]], [actual.get(ctx.file.request.path)], "runner alias reaches the canonical File from the trace")
    return analysistest.end(env)

_runner_alias_test = analysistest.make(
    _runner_alias_impl,
    extra_target_under_test_aspects = [runnable_action_aspect],
    attrs = {
        "case": attr.string(mandatory = True),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "request": attr.label(allow_single_file = True, mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _config_value(env, content, name):
    prefix = "const " + name + " = "
    values = [json.decode(line[len(prefix):-1]) for line in content.splitlines() if line.startswith(prefix) and line.endswith(";")]
    asserts.equals(env, 1, len(values), "actual Vitest config " + name)
    return values[0] if len(values) == 1 else None

def _roots_impl(ctx):
    env = analysistest.begin(ctx)
    case = json.decode(ctx.attr.case)
    facts = case["input"]
    expected = case["expected"]
    target = analysistest.target_under_test(env)
    runfiles = target[DefaultInfo].default_runfiles
    record = _written_action(env, ".producer_roots.json")
    selected_file = _written_action(env, runnable_action_label(env).name + "_test_files.txt")
    launcher_file = _written_action(env, "_launcher.json")
    local = [file for file in runfiles.files.to_list() if file == ctx.file.local_source]
    asserts.equals(env, 1, len(local), "bind the actual local source-mode JavaScript File")
    if not record or not selected_file or not launcher_file or len(local) != 1:
        return analysistest.end(env)
    ids = {
        "source": ctx.file.source,
        "runtime": _runtime(ctx.attr.producer[TsInfo], ctx.file.source),
        "local_source": ctx.file.local_source,
        "local_runtime": local[0],
    }
    ids[facts["request"]] = ctx.file.request
    ids.update({identity: file for identity, file in zip(facts["extra_inputs"], ctx.files.extra_inputs)})
    actual = json.decode(record.content)
    selected = {ids[runtime]: True for _request, runtime in expected["selected"]}
    asserts.equals(env, {file.path: True for file in selected}, {path: True for path in actual["entry_points"]}, "requested roots reach the real runner callback")
    asserts.true(env, actual["runtime_inputs"] != None, "runner callback exposes its requested input projection")
    if actual["runtime_inputs"] != None:
        asserts.equals(env, {(ids[request].path, ids[runtime].path): True for request, runtime in expected["selected"]}, {(request, runtime): True for request, runtime in actual["runtime_inputs"]}, "requested input projection remains separate from producing origins")
    asserts.equals(env, {rlocation_path(ctx, file): True for file in selected}, {path: True for path in selected_file.content.splitlines() if path}, "ts_test writes every requested runtime root")
    runtime_paths = {file.path: True for file in selected}
    asserts.equals(env, {(ids[source].path, ids[runtime].path): True for source, runtime in expected["runner_origins"]}, {(source, runtime): True for source, runtime in actual["runtime_files"] if runtime in runtime_paths}, "runner mappings retain producing facts without invented opaque origins")
    launcher = json.decode(launcher_file.content)
    asserts.equals(env, facts["runner"], launcher["mode"], "actual native runner mode")
    if facts["runner"] == "node_test":
        asserts.equals(env, rlocation_path(ctx, selected_file.outputs.to_list()[0]), launcher["node_test"]["test_files_list"], "Node consumes the selected root list")
    else:
        config = _written_action(env, "_selected_roots.vitest/config.mjs")
        discovery = _written_action(env, "_selected_roots.vitest/test_files.txt")
        if not config or not discovery:
            return analysistest.end(env)
        prefix = _config_value(env, config.content, "DISCOVERY_PREFIX")
        references = _config_value(env, config.content, "MODULE_REFERENCES")
        if prefix == None or references == None:
            return analysistest.end(env)
        wanted = {prefix + "/" + rlocation_path(ctx, ids[source]): ids[runtime] for source, runtime in expected["discovery"]}
        asserts.equals(env, {path: True for path in wanted}, {path: True for path in discovery.content.splitlines() if path}, "Vitest discovers each explicitly requested input coordinate")
        links = {runfiles_root_path(ctx, link.path): link.target_file for link in runfiles.symlinks.to_list()}
        asserts.equals(env, wanted, {path: links.get(path) for path in wanted}, "Vitest discovery links retain the selected runtime Files")
        runtime_locations = {rlocation_path(ctx, file): True for file in selected}
        asserts.equals(env, {(rlocation_path(ctx, ids[source]), rlocation_path(ctx, ids[runtime])): True for source, runtime in expected["runner_origins"]}, {(row[1], row[2]): True for row in references if row[2] in runtime_locations}, "Vitest module references retain producing facts independently of discovery")
        asserts.equals(env, rlocation_path(ctx, discovery.outputs.to_list()[0]), launcher["vitest"]["test_files_list"], "Vitest consumes its actual discovery list")
    return analysistest.end(env)

_roots_test = analysistest.make(
    _roots_impl,
    extra_target_under_test_aspects = [runnable_action_aspect],
    attrs = {
        "case": attr.string(mandatory = True),
        "producer": attr.label(providers = [TsInfo], mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
        "local_source": attr.label(allow_single_file = True, mandatory = True),
        "request": attr.label(allow_single_file = True, mandatory = True),
        "extra_inputs": attr.label_list(allow_files = True),
    },
)

def _label(path, name):
    return "//" + "/".join(path) + ":" + name

def producer_fixture(case, runner_bindings):
    facts = case["input"]
    source = _label(facts["producer_package"], "/".join(facts["source_path"][len(facts["producer_package"]):]))
    if facts["kind"] == "generated_js":
        ts_codegen(
            name = "producer",
            srcs = ["producer.input.js"],
            outs = ["/".join(facts["source_path"][len(facts["producer_package"]):])],
            generator = "@rules_typescript//ts/tools/tsaction:tsaction",
            args = ["stage", "-out={outs_dir}", "{srcs}", "/".join(facts["source_path"][len(facts["producer_package"]):])],
            visibility = ["//visibility:public"],
        )
    elif facts["kind"] == "opaque":
        _opaque(name = "producer", source = source, facts = json.encode(facts), visibility = ["//visibility:public"])
    else:
        ts_compile(
            name = "producer",
            srcs = [] if facts["kind"] in ["asset", "asset_js"] else [source],
            data = [source] if facts["kind"] in ["asset", "asset_js"] else [],
            emit = facts["kind"] == "module",
            package_scopes = ["package.json"] if facts["scope"] else [],
            node_modules = runner_bindings["node_modules"] if facts["runner"] else None,
            deps = [runner_bindings["node_types"]] + ([runner_bindings["vitest"]] if facts["runner"] == "vitest" else []) if facts["runner"] else [],
            visibility = ["//visibility:public"],
        )
    if facts["peer"]:
        ts_compile(name = "peer", srcs = [source], visibility = ["//visibility:public"])
    _primary(name = "primary", producer = ":producer", source = source, visibility = ["//visibility:public"])
    if facts["injected"]:
        _facts(name = "facts", producer = ":producer", source = source, other_source = "other.ts", facts = json.encode(facts), visibility = ["//visibility:public"])
        for identity in facts["extra_inputs"]:
            native.filegroup(name = "input_" + identity, srcs = [":facts"], output_group = identity, visibility = ["//visibility:public"])
    if facts["repeat"]:
        native.alias(name = "repeated", actual = ":producer", visibility = ["//visibility:public"])

def consumer_fixture(case, runner_bindings):
    facts = case["input"]
    producer = _label(facts["producer_package"], "producer")
    mutated = facts["injected"]
    request = _label(facts["producer_package"], "facts" if mutated else "primary")
    deps = [_label(facts["producer_package"], "facts")] if mutated else [producer]
    if facts["peer"]:
        deps.append(_label(facts["producer_package"], "peer"))
    if facts["repeat"]:
        deps.append(_label(facts["producer_package"], "repeated"))
    if facts["runner"]:
        deps.append(runner_bindings["node_types"])
        if facts["runner"] == "vitest":
            deps.append(runner_bindings["vitest"])
    foreign = [_label(path[:-1], path[-1]) for path in facts["foreign_sources"]]
    data = [request] if facts["edge"] == "data" else []
    if facts["shadow"]:
        data.append("nested/package.json")

    # Expected-failure subjects must only be reached through their analysis-test transition.
    ts_compile(
        name = "consumer",
        srcs = ([request] if facts["edge"] == "srcs" else []) + foreign,
        data = data,
        deps = deps,
        emit = bool(foreign),
        node_modules = runner_bindings["node_modules"] if facts["runner"] else None,
        tags = ["manual"] if case["phase"] == "rejected" else [],
    )
    _probe(name = "admission", subject = ":consumer", tags = ["manual"] if case["phase"] == "rejected" else [])
    if case["phase"] == "rejected":
        _rejected_test(name = "replay_test", target_under_test = ":admission", case = json.encode(case))
    else:
        _accepted_test(
            name = "replay_test",
            target_under_test = ":admission",
            case = json.encode(case),
            producer = producer,
            peer = _label(facts["producer_package"], "peer") if facts["peer"] else None,
            source = _label(facts["producer_package"], "/".join(facts["source_path"][len(facts["producer_package"]):])),
        )
    if facts["name"] == "composed_alias_data":
        ts_binary(name = "native_alias", entry_point = request, data = [request])
        _native_alias_test(
            name = "native_alias_replay_test",
            target_under_test = ":native_alias",
            case = json.encode(case),
            producer = producer,
            request = request,
            source = _label(facts["producer_package"], "/".join(facts["source_path"][len(facts["producer_package"]):])),
        )
        _alias_data(name = "alias_data", request = request)
        _observed_runner(name = "alias_runner", original = "@rules_typescript//ts/runners:vitest")
        ts_test(
            name = "runner_alias",
            srcs = [request],
            deps = [":alias_data", runner_bindings["node_types"], runner_bindings["vitest"]],
            node_modules = runner_bindings["node_modules"],
            runner = ":alias_runner",
        )
        _runner_alias_test(
            name = "runner_alias_replay_test",
            target_under_test = ":runner_alias",
            case = json.encode(case),
            producer = producer,
            request = request,
            source = _label(facts["producer_package"], "/".join(facts["source_path"][len(facts["producer_package"]):])),
        )
    if facts["runner"]:
        local = _label(facts["consumer_package"], "/".join(facts["local_path"][len(facts["consumer_package"]):]))
        roots = {facts["request"]: request, "local_source": local}
        extra_inputs = [_label(facts["producer_package"], "input_" + identity) for identity in facts["extra_inputs"]]
        roots.update({identity: label for identity, label in zip(facts["extra_inputs"], extra_inputs)})
        _observed_runner(name = "observed_runner", original = "@rules_typescript//ts/runners:" + facts["runner"])
        ts_test(
            name = "selected_roots",
            srcs = [request, local] + extra_inputs,
            test_srcs = [roots[identity] for identity in facts["roots"]],
            deps = deps,
            node_modules = runner_bindings["node_modules"],
            runner = ":observed_runner",
        )
        _roots_test(
            name = "roots_replay_test",
            target_under_test = ":selected_roots",
            case = json.encode(case),
            producer = producer,
            source = _label(facts["producer_package"], "/".join(facts["source_path"][len(facts["producer_package"]):])),
            local_source = local,
            request = request,
            extra_inputs = extra_inputs,
        )
