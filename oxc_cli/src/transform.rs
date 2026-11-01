//! Per-file transformation pipeline.
//!
//! Each call to [`transform_file`] is independent and safe to run on any
//! Rayon thread.  The function:
//!
//! 1. Reads the source file from disk.
//! 2. Parses it with `oxc_parser`.
//! 3. Runs semantic analysis with `oxc_semantic` to produce a `Scoping`.
//! 4. Runs `oxc_transformer` to strip TypeScript and transform JSX/ESNext.
//! 5. Runs `oxc_isolated_declarations` to emit a `.d.ts` AST (if requested).
//! 6. Uses `oxc_codegen` to serialise both the JS output and the `.d.ts`
//!    output to strings, optionally attaching source maps.
//! 7. Writes all outputs to the computed paths under `out_dir`.

use std::{
    fs,
    path::{Path, PathBuf},
};

use miette::{IntoDiagnostic, NamedSource, SourceSpan, miette};
use oxc_allocator::Allocator;
use oxc_codegen::{Codegen, CodegenOptions};
use oxc_isolated_declarations::{IsolatedDeclarations, IsolatedDeclarationsOptions};
use oxc_parser::Parser;
use oxc_semantic::SemanticBuilder;
use oxc_span::{GetSpan, SourceType};
use oxc_transformer::{JsxOptions, JsxRuntime, TransformOptions, Transformer, TypeScriptOptions};

use crate::options::CliOptions;

#[cfg(test)]
#[path = "transform_tests.rs"]
mod tests;

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

/// Transform a single TypeScript/TSX file and write its outputs.
///
/// Returns `Ok(())` on success.  On failure, returns a [`miette::Report`]
/// with source context and span information where available.
pub fn transform_file(input_path: &Path, opts: &CliOptions) -> miette::Result<()> {
    // ── 1. Read source ────────────────────────────────────────────────────
    let source_text = fs::read_to_string(input_path)
        .into_diagnostic()
        .map_err(|e| miette!("Failed to read {}: {e}", input_path.display()))?;

    // ── 2. Compute output paths ───────────────────────────────────────────
    let output_paths = compute_output_paths(input_path, opts)?;

    // ── 3. Determine SourceType ───────────────────────────────────────────
    let source_type = SourceType::from_path(input_path).map_err(|e| {
        miette!(
            "Unsupported file extension for {}: {}",
            input_path.display(),
            e
        )
    })?;

    // ── 4. Build TransformOptions ─────────────────────────────────────────
    let transform_options = build_transform_options(opts)?;

    // ── 5. Allocate arena, parse, analyse, transform, codegen ─────────────
    // All oxc arena-allocated values share a single allocator that is dropped
    // together at the end of this function, so no lifetime escapes.
    let allocator = Allocator::new();

    let (js_code, js_map, dts_code) = {
        // Parse
        let mut parse_ret =
            Parser::new(&allocator, &source_text, source_type).parse();

        if parse_ret.fatal_error {
            return Err(make_parse_error(
                input_path,
                &source_text,
                &parse_ret.diagnostics,
                "Parser panicked on unrecoverable syntax error",
            ));
        }
        if !parse_ret.diagnostics.is_empty() {
            // Treat any parse error as fatal; non-fatal diagnostics would
            // still be surfaced here so the user can fix them.
            return Err(make_parse_error(
                input_path,
                &source_text,
                &parse_ret.diagnostics,
                "Syntax error(s)",
            ));
        }

        // Semantic analysis – produces the Scoping the transformer needs.
        let semantic_ret = SemanticBuilder::new()
            .with_check_syntax_error(true)
            .build(&parse_ret.program);

        if !semantic_ret.diagnostics.is_empty() {
            return Err(make_parse_error(
                input_path,
                &source_text,
                &semantic_ret.diagnostics,
                "Semantic error(s)",
            ));
        }

        let scoping = semantic_ret.semantic.into_scoping();

        // ── Optional: generate isolated declarations BEFORE transforming
        // the AST (the transformer mutates it in place, removing type info).
        //
        // When --isolated-declarations is set we enforce strict isolated-
        // declarations semantics: missing return types are a hard error.
        //
        // When only --declaration is set (isolated_declarations = false in
        // the Bazel rule) we still use IsolatedDeclarations to emit the .d.ts
        // but we do NOT propagate errors — this lets legacy code without
        // explicit return types compile successfully.
        let dts_code = if opts.emit_declarations() {
            let id_ret = IsolatedDeclarations::new(
                &allocator,
                IsolatedDeclarationsOptions { strip_internal: false },
            )
            .build(&parse_ret.program);

            if opts.isolated_declarations && !id_ret.diagnostics.is_empty() {
                return Err(make_parse_error(
                    input_path,
                    &source_text,
                    &id_ret.diagnostics,
                    "Isolated declarations error(s)",
                ));
            }

            // Codegen for .d.ts — source maps are not useful here.
            let dts_cg = Codegen::new()
                .with_options(CodegenOptions::default())
                .build(&id_ret.program);

            Some(dts_cg.code)
        } else {
            None
        };

        let starts_before: Vec<u32> =
            parse_ret.program.body.iter().map(|s| s.span().start).collect();

        // Transform (mutates parse_ret.program in place).
        let transformer_ret = Transformer::new(
            &allocator,
            input_path,
            &transform_options,
        )
        .build_with_scoping(scoping, &mut parse_ret.program);

        if !transformer_ret.diagnostics.is_empty() {
            return Err(make_parse_error(
                input_path,
                &source_text,
                &transformer_ret.diagnostics,
                "Transform error(s)",
            ));
        }

        // A comment above an erased statement moves to the next kept one: a
        // leading docblock outlives the `import type` it sits on.
        let starts_after: Vec<u32> =
            parse_ret.program.body.iter().map(|s| s.span().start).collect();
        let end = parse_ret.program.span.end;
        for comment in parse_ret.program.comments.iter_mut() {
            let at = comment.attached_to;
            if starts_before.contains(&at) && !starts_after.contains(&at) {
                comment.attached_to = starts_after
                    .iter()
                    .copied()
                    .filter(|&s| s > at)
                    .min()
                    .unwrap_or(end);
            }
        }

        // Codegen for .js — optionally emit a source map.
        let js_codegen_opts = CodegenOptions {
            source_map_path: if opts.source_map {
                Some(input_path.to_path_buf())
            } else {
                None
            },
            ..CodegenOptions::default()
        };

        let js_cg = Codegen::new()
            .with_options(js_codegen_opts)
            .with_source_text(&source_text)
            .build(&parse_ret.program);

        let js_map = js_cg.map.map(|m| m.to_json_string());
        (js_cg.code, js_map, dts_code)
    };

    // ── 6. Write outputs ──────────────────────────────────────────────────
    // Ensure parent directories exist.
    if let Some(parent) = output_paths.js.parent() {
        fs::create_dir_all(parent)
            .into_diagnostic()
            .map_err(|e| miette!("Failed to create output directory {}: {e}", parent.display()))?;
    }

    fs::write(&output_paths.js, &js_code)
        .into_diagnostic()
        .map_err(|e| miette!("Failed to write {}: {e}", output_paths.js.display()))?;

    if let (Some(map_str), Some(map_path)) = (js_map, &output_paths.js_map) {
        fs::write(map_path, &map_str)
            .into_diagnostic()
            .map_err(|e| miette!("Failed to write {}: {e}", map_path.display()))?;
    }

    if let (Some(dts), Some(dts_path)) = (dts_code, &output_paths.dts) {
        fs::write(dts_path, &dts)
            .into_diagnostic()
            .map_err(|e| miette!("Failed to write {}: {e}", dts_path.display()))?;
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// Output path computation
// ---------------------------------------------------------------------------

struct OutputPaths {
    js: PathBuf,
    js_map: Option<PathBuf>,
    dts: Option<PathBuf>,
}

fn compute_output_paths(input_path: &Path, opts: &CliOptions) -> miette::Result<OutputPaths> {
    // Strip the prefix (if given) to get the relative part of the path.
    let relative = match &opts.strip_dir_prefix {
        Some(prefix) => input_path.strip_prefix(prefix).unwrap_or(input_path),
        None => input_path,
    };

    // Strip the TypeScript extension (.ts or .tsx) from the filename to get
    // the stem used for all output paths.
    //
    // We intentionally do NOT use Path::file_stem() + Path::with_extension()
    // because file_stem() only strips the *last* extension component, and
    // with_extension() also only replaces the last component.  For a file like
    // "math.test.ts", file_stem() → "math.test", then with_extension("js") →
    // "math.js", which is wrong.  We want "math.test.js".
    //
    // Instead, strip the known TypeScript extension from the full filename as a
    // string, then join the new extension with a dot.
    let file_name = relative
        .file_name()
        .ok_or_else(|| miette!("Input path has no file name: {}", input_path.display()))?
        .to_string_lossy();

    let is_tsx = file_name.ends_with(".tsx");
    let stem = if is_tsx {
        &file_name[..file_name.len() - 4]
    } else if file_name.ends_with(".ts") {
        &file_name[..file_name.len() - 3]
    } else {
        return Err(miette!(
            "Input file does not have a .ts or .tsx extension: {}",
            input_path.display()
        ));
    };

    let parent = relative.parent().unwrap_or(Path::new(""));
    let base_dir = opts.out_dir.join(parent);

    // tsc's naming: a .tsx whose JSX is preserved keeps the x.
    let js_extension = if is_tsx && opts.jsx == "preserve" {
        "jsx"
    } else {
        "js"
    };
    let js_path = base_dir.join(format!("{stem}.{js_extension}"));
    let js_map_path = if opts.source_map {
        Some(base_dir.join(format!("{stem}.{js_extension}.map")))
    } else {
        None
    };
    let dts_path = if opts.emit_declarations() {
        Some(base_dir.join(format!("{stem}.d.ts")))
    } else {
        None
    };

    Ok(OutputPaths {
        js: js_path,
        js_map: js_map_path,
        dts: dts_path,
    })
}

// ---------------------------------------------------------------------------
// TransformOptions construction
// ---------------------------------------------------------------------------

fn build_transform_options(opts: &CliOptions) -> miette::Result<TransformOptions> {
    // Map our --jsx flag to a TransformOptions configuration.
    // "preserve" means we keep JSX syntax as-is (no jsx_plugin).
    // "react-jsx" and "react" both enable the jsx_plugin but differ in runtime.
    let jsx_opts = build_jsx_options(opts)?;

    // Map --target to TransformOptions via from_target.
    let mut transform_opts = TransformOptions::from_target(&opts.target)
        .map_err(|e| miette!("Invalid --target value '{}': {e}", opts.target))?;

    transform_opts.jsx = jsx_opts;
    transform_opts.typescript = TypeScriptOptions::default();
    if opts.top_level_await {
        // oxc's top_level_await is the refusal below es2022, not a lowering.
        transform_opts.env.es2022.top_level_await = false;
    }

    Ok(transform_opts)
}

fn build_jsx_options(opts: &CliOptions) -> miette::Result<JsxOptions> {
    match opts.jsx.as_str() {
        "preserve" => {
            // Leave JSX in the output unchanged.
            Ok(JsxOptions::disable())
        }
        "react" => {
            // Classic runtime: React.createElement calls.
            let mut jsx = JsxOptions::enable();
            jsx.runtime = JsxRuntime::Classic;
            Ok(jsx)
        }
        "react-jsx" => {
            // Automatic runtime (React 17+): imports from jsx-runtime.
            let mut jsx = JsxOptions::enable();
            jsx.runtime = JsxRuntime::Automatic;
            if let Some(src) = &opts.jsx_import_source {
                jsx.import_source = Some(src.clone());
            }
            Ok(jsx)
        }
        other => Err(miette!(
            "Unknown --jsx mode '{}'. Valid values: react-jsx, react, preserve",
            other
        )),
    }
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

fn make_parse_error(
    path: &Path,
    source: &str,
    diagnostics: &[oxc_diagnostics::OxcDiagnostic],
    headline: &str,
) -> miette::Report {
    // Build a combined message from all diagnostics.  For the first diagnostic
    // that carries span information we surface the source snippet via miette's
    // NamedSource.
    let messages: Vec<String> = diagnostics
        .iter()
        .map(|d| format!("{d}"))
        .collect();
    let combined = messages.join("\n");

    // Try to extract a span from the first diagnostic for inline highlighting.
    let span: Option<SourceSpan> = diagnostics.first().and_then(|d| {
        d.labels.first().map(|label| {
            SourceSpan::new((label.offset() as usize).into(), label.len() as usize)
        })
    });

    let named_source = NamedSource::new(path.to_string_lossy(), source.to_owned());

    match span {
        Some(sp) => miette!(
            labels = vec![miette::LabeledSpan::at(sp, "here")],
            "{headline}: {combined}"
        )
        .with_source_code(named_source),
        None => miette!("{headline} in {}: {combined}", path.display()),
    }
}
