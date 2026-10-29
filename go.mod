module github.com/mikn/rules_typescript

go 1.26.1

// Bazel resolves github.com/bazelbuild/rules_go/go/runfiles by label, not by
// module; this require exists only so go vet / gopls / staticcheck see the code.
require github.com/bazelbuild/rules_go v0.60.0

// bufrequest's protobuf-go, the version tests/proto's buf golden was made with.
require google.golang.org/protobuf v1.36.12
