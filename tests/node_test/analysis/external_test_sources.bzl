def _external_test_sources_impl(rctx):
    package = "tests/node_test/analysis/"
    files = ["external_sole.test.js", "external_mixed.test.js"]
    for name in files:
        rctx.file(package + name, """import { test } from 'node:test';
import { strictEqual } from 'node:assert';
import { createRequire } from 'node:module';
const value = createRequire(import.meta.url)('./external_value.json').value;
test('%s keeps its external JSON sibling', () => strictEqual(value, 42));
""" % name)
    files.append("external_value.json")
    rctx.file(package + "external_value.json", '{"value":42}\n')
    rctx.file(package + "BUILD.bazel", "exports_files(" + repr(files) + ", visibility = ['//visibility:public'])\n")

external_test_sources = repository_rule(implementation = _external_test_sources_impl)
