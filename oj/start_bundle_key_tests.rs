// oj_cache (lovablelabs/oj#348): with the app reached through a symlink, the
// client bundle closure listed oj's own start/manifest.ts by its realpath, so
// the placeholder every boot writes there changed the key and no restart hit.

use std::fs;
use std::path::{Path, PathBuf};

use oj_cache::integrity::VerifyMode;
use oj_cache::start_bundle::{Miss, StartBundleStore};

const DEV_MANIFEST: &str = "export const tsrStartManifest = () => ({\"routes\":{}});\n";
const PLACEHOLDER_MANIFEST: &str = "export const tsrStartManifest = () => ({ routes: {} });\n";

struct App {
    real: PathBuf,
    link: PathBuf,
}

impl App {
    fn new() -> App {
        let base = std::env::var_os("TEST_TMPDIR")
            .map(PathBuf::from)
            .unwrap_or_else(std::env::temp_dir)
            .join(format!("start-bundle-key-{}", std::process::id()));
        fs::create_dir_all(base.join("real/src")).unwrap();
        let real = fs::canonicalize(base.join("real")).unwrap();
        let link = base.join("link");
        std::os::unix::fs::symlink(&real, &link).unwrap();
        fs::write(real.join("package.json"), "{}").unwrap();
        fs::write(real.join("src/app.tsx"), "export const app = 1;\n").unwrap();
        App { real, link }
    }

    fn start_dir(&self) -> PathBuf {
        oj_cache::cache_root(&self.link).join("start")
    }

    /// What bundle-client.mjs leaves in the start dir: the bundler reports
    /// closure ids by realpath, the start dir's manifest.ts among them.
    fn write_build(&self) {
        let start = self.start_dir();
        let chunks = start.join("client-chunks");
        fs::create_dir_all(&chunks).unwrap();
        fs::write(chunks.join("client-entry.js"), "console.log(1);\n").unwrap();
        fs::write(
            start.join("client-chunks.json"),
            r#"{"entry":"client-entry.js","files":[{"name":"client-entry.js","size":16}]}"#,
        )
        .unwrap();
        fs::write(start.join("client-entry.modules"), "1").unwrap();
        fs::write(start.join("manifest.ts"), DEV_MANIFEST).unwrap();
        let closure = [
            self.real.join("src/app.tsx"),
            fs::canonicalize(start.join("manifest.ts")).unwrap(),
        ];
        let json: Vec<String> = closure
            .iter()
            .map(|p| format!("\"{}\"", p.display()))
            .collect();
        fs::write(start.join("closure.json"), format!("[{}]", json.join(","))).unwrap();
    }

    fn store(&self) -> StartBundleStore {
        StartBundleStore::new(&self.link, "0.0.0-test", VerifyMode::Standard)
    }
}

fn reboot(start: &Path) {
    // oj_server::write_start_assets on every boot, before the restore.
    fs::write(start.join("manifest.ts"), PLACEHOLDER_MANIFEST).unwrap();
}

#[test]
fn a_restart_restores_the_client_bundle_despite_the_rewritten_start_manifest() {
    let app = App::new();
    app.write_build();
    let start = app.start_dir();
    let (key, _) = app.store().persist(&start).expect("persist");

    reboot(&start);
    match app.store().restore(&start) {
        Ok((stats, _)) => assert_eq!(stats.key, key),
        Err(miss) => panic!("restart missed the bundle it just cached: {miss}"),
    }

    // The app's own sources still key the bundle.
    fs::write(app.real.join("src/app.tsx"), "export const app = 2;\n").unwrap();
    reboot(&start);
    let miss = app.store().restore(&start).err();
    assert!(
        matches!(miss, Some(Miss::NoEntryForKey(_))),
        "an edited source must miss, got {miss:?}"
    );
}
