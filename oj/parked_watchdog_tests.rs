// oj_js (lovablelabs/oj#347): a parked call's watchdog fired at the call's
// own deadline and terminated whichever call's JavaScript was on the stack.

use std::path::PathBuf;
use std::time::Duration;

use oj_js::EngineConfig;
use oj_js::EngineError;
use oj_js::JsEngine;

const MODULE: &str = r#"
export function park() { return new Promise(() => {}); }
export function spin(ms) {
  const end = Date.now() + ms;
  let n = 0;
  while (Date.now() < end) n++;
  return n > 0 ? "spun" : "never";
}
export function ok() { return "alive"; }
"#;

fn app_root() -> PathBuf {
    let base = std::env::var_os("TEST_TMPDIR")
        .map(PathBuf::from)
        .unwrap_or_else(std::env::temp_dir);
    let root = base.join(format!("parked-watchdog-{}", std::process::id()));
    std::fs::create_dir_all(&root).unwrap();
    std::fs::write(root.join("package.json"), r#"{"name":"fixture","version":"1.0.0"}"#).unwrap();
    std::fs::write(root.join("calls.mjs"), MODULE).unwrap();
    root
}

#[tokio::test]
async fn parked_deadline_does_not_terminate_another_calls_javascript() {
    let root = app_root();
    let engine = std::sync::Arc::new(JsEngine::spawn(EngineConfig::new(&root), None, None).unwrap());
    // Loads and evaluates the module, so the parked call's setup is fast.
    let warm = engine.call("calls.mjs", "ok", vec![], None).await.unwrap();
    assert_eq!(warm.as_str(), Some("alive"));

    let parked = {
        let engine = engine.clone();
        tokio::spawn(async move {
            engine
                .call("calls.mjs", "park", vec![], Some(Duration::from_millis(500)))
                .await
        })
    };
    tokio::time::sleep(Duration::from_millis(100)).await;
    // No deadline of its own: it is still on the stack when the parked call's
    // deadline passes at 500 ms.
    let spun = tokio::time::timeout(
        Duration::from_secs(30),
        engine.call("calls.mjs", "spin", vec![2000.into()], None),
    )
    .await
    .expect("the spinning call never returned");
    let parked = tokio::time::timeout(Duration::from_secs(30), parked)
        .await
        .expect("the parked call never settled")
        .unwrap();

    match spun {
        Ok(value) => assert_eq!(value.as_str(), Some("spun")),
        Err(e) => panic!("the spinning call failed with {e:?}: the parked call's watchdog terminated it"),
    }
    assert!(
        matches!(parked, Err(EngineError::Deadline)),
        "the parked call must expire with Deadline, got {parked:?}"
    );
    let after = engine.call("calls.mjs", "ok", vec![], None).await.unwrap();
    assert_eq!(after.as_str(), Some("alive"));
}
