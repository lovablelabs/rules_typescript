module github.com/mikn/rules_typescript

go 1.27.3

require (
	github.com/bazelbuild/bazel-gazelle v0.54.0
	github.com/bazelbuild/buildtools v0.0.0-20260904073137-eaa4d125b423
	// Bazel resolves github.com/bazelbuild/rules_go/go/runfiles by label, not by
	// module; this require exists only so go vet / gopls / staticcheck see the code.
	github.com/bazelbuild/rules_go v0.65.0
	// bufrequest's protobuf-go, the version tests/proto's buf golden was made with.
	google.golang.org/protobuf v1.36.12
)

require github.com/bazel-contrib/bazel-gazelle/v2 v2.0.0-3 // indirect
