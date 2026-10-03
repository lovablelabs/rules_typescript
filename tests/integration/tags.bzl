"""The tags every nested-Bazel integration test carries.

One definition rather than one per package: three hand-copied copies had already
drifted, and the copy nobody remembered kept `exclusive` after the others
dropped it -- which put it back in the fast lane on its own.
"""

load(
    "@rules_bazel_integration_test//bazel_integration_test:defs.bzl",
    "integration_test_utils",
)

_BASE_TAGS = [
    tag
    for tag in integration_test_utils.DEFAULT_INTEGRATION_TEST_TAGS
    if tag not in ("manual", "exclusive")
] + [
    # Lane membership, kept separate from scheduling: this is what
    # --test_tag_filters=-nested-bazel excludes from the fast lane. `exclusive`
    # used to do both jobs, so dropping it silently pulled 18 nested Bazel
    # invocations into the unit-test job.
    "nested-bazel",
    "no-sandbox",
    # These point a nested Bazel at this source tree via RULES_TS_ROOT, so the
    # ruleset's .bzl files are read without being action inputs -- and glob()
    # cannot collect them into one filegroup, since every dir is a subpackage.
    "external",
]

# The core CI filter includes every test without a named shard.
SHARDS = [
    "npm",
]

def nested_bazel_tags(shard = None, cpus = 2):
    """Tags for one nested-Bazel integration test."""
    tags = _BASE_TAGS + ["cpu:%d" % cpus]
    if shard == None:
        return tags
    if shard not in SHARDS:
        fail(
            "unknown integration shard %r. Did you mean one of %s? A new leg " % (shard, SHARDS) +
            "needs three edits: the name here, a test:ci-integration-<name> " +
            "config in //.bazelrc (and its exclusion from the core leg's " +
            "filter), and a matrix entry in .github/workflows/ci.yml.",
        )
    return tags + ["shard-" + shard]
