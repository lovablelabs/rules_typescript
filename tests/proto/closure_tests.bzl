"""The files ts_proto_closure generates for tests/proto:closure."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")

def _closure_files_test_impl(ctx):
    env = analysistest.begin(ctx)
    files = analysistest.target_under_test(env)[DefaultInfo].files.to_list()
    asserts.equals(
        env,
        ["closure/proof/v1/common_pb.ts", "closure/proof/v1/message_pb.ts", "closure/proof/v1/service_pb.ts"],
        sorted([file.short_path.removeprefix("tests/proto/") for file in files]),
    )
    return analysistest.end(env)

closure_files_test = analysistest.make(_closure_files_test_impl)
