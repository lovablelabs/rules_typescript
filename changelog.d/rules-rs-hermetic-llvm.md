### Breaking — native toolchains

- Native tools use rules_rs 0.0.111 and hermetic LLVM 0.8.21. Cargo.toml and Cargo.lock define both crate closures directly; vendored cargo-bazel files and the rules_rust compatibility layer are removed. Consumers that configure the shared default Rust toolchain must match edition 2024 and Rust 1.99.1. See [Compatibility](../COMPATIBILITY.md#rust-and-cc-toolchains).
