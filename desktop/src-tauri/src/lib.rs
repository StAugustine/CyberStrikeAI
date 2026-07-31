use serde::Deserialize;
use std::{
    sync::{
        atomic::{AtomicBool, AtomicI32, Ordering},
        Mutex,
    },
    thread,
    time::{Duration, Instant},
};
use tauri::{
    webview::{DownloadEvent, NewWindowResponse},
    AppHandle, Manager, RunEvent, WebviewUrl, WebviewWindowBuilder,
};
use tauri_plugin_shell::{process::CommandChild, process::CommandEvent, ShellExt};

const SIDECAR_NAME: &str = "cyberstrike-go-poc";
const READY_PROTOCOL_VERSION: u32 = 1;
const FORCE_EXIT_TIMEOUT: Duration = Duration::from_secs(5);
static DESIRED_EXIT_CODE: AtomicI32 = AtomicI32::new(0);

#[derive(Default)]
struct SidecarState {
    child: Mutex<Option<CommandChild>>,
    allowed_origin: Mutex<Option<String>>,
    shutting_down: AtomicBool,
    failed: AtomicBool,
    protocol_verified: AtomicBool,
    download_intercepted: AtomicBool,
}

#[derive(Debug, Deserialize)]
struct ReadyMessage {
    #[serde(rename = "type")]
    kind: String,
    protocol_version: u32,
    url: String,
    app_version: String,
}

#[derive(Debug, Deserialize)]
struct PocResultMessage {
    #[serde(rename = "type")]
    kind: String,
    rest: bool,
    sse: bool,
    websocket: bool,
    external_navigation_blocked: bool,
}

pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            eprintln!("desktop PoC existing instance focused");
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.unminimize();
                let _ = window.show();
                let _ = window.set_focus();
            }
        }))
        .plugin(tauri_plugin_shell::init())
        .manage(SidecarState::default())
        .setup(|app| {
            let navigation_handle = app.handle().clone();
            let download_handle = app.handle().clone();
            WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("CyberStrikeAI Desktop PoC")
                .inner_size(960.0, 720.0)
                .min_inner_size(720.0, 520.0)
                .center()
                .on_navigation(move |url| navigation_allowed(&navigation_handle, url))
                .on_new_window(|_url, _features| NewWindowResponse::Deny)
                .on_download(move |_webview, event| match event {
                    DownloadEvent::Requested { url, .. } => {
                        let allowed = url.path() == "/api/poc/download"
                            && navigation_allowed(&download_handle, &url);
                        if allowed {
                            eprintln!("desktop PoC download intercepted");
                            download_handle
                                .state::<SidecarState>()
                                .download_intercepted
                                .store(true, Ordering::SeqCst);
                        }
                        false
                    }
                    _ => true,
                })
                .build()?;
            start_sidecar(app.handle())?;
            #[cfg(debug_assertions)]
            schedule_automatic_exit(app.handle());
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build CyberStrikeAI desktop PoC");

    let exit_code = app.run_return(|handle, event| {
        if let RunEvent::ExitRequested { api, .. } = event {
            if handle
                .state::<SidecarState>()
                .shutting_down
                .load(Ordering::SeqCst)
            {
                return;
            }
            api.prevent_exit();
            request_shutdown(handle);
        }
    });
    let desired_exit_code = DESIRED_EXIT_CODE.load(Ordering::SeqCst);
    std::process::exit(if desired_exit_code == 0 {
        exit_code
    } else {
        desired_exit_code
    });
}

fn start_sidecar(handle: &AppHandle) -> Result<(), Box<dyn std::error::Error>> {
    let (mut events, child) = handle.shell().sidecar(SIDECAR_NAME)?.spawn()?;
    handle
        .state::<SidecarState>()
        .child
        .lock()
        .map_err(|_| "sidecar state lock poisoned")?
        .replace(child);

    let task_handle = handle.clone();
    tauri::async_runtime::spawn(async move {
        let mut ready = false;
        while let Some(event) = events.recv().await {
            match event {
                CommandEvent::Stdout(line) if !ready => match parse_ready(&line) {
                    Ok(url) => {
                        ready = true;
                        let origin = url.origin().ascii_serialization();
                        let state = task_handle.state::<SidecarState>();
                        let Ok(mut allowed_origin) = state.allowed_origin.lock() else {
                            fail_sidecar(&task_handle, "allowed origin state lock poisoned");
                            return;
                        };
                        allowed_origin.replace(origin);
                        drop(allowed_origin);
                        if let Some(window) = task_handle.get_webview_window("main") {
                            if let Err(error) = window.navigate(url) {
                                fail_sidecar(
                                    &task_handle,
                                    &format!("navigate to sidecar: {error}"),
                                );
                                return;
                            }
                        } else {
                            fail_sidecar(&task_handle, "main window is missing");
                            return;
                        }
                    }
                    Err(error) => {
                        fail_sidecar(&task_handle, &error);
                        return;
                    }
                },
                CommandEvent::Stdout(line) => match parse_poc_result(&line) {
                    Ok(()) => {
                        eprintln!("desktop PoC browser protocols verified");
                        task_handle
                            .state::<SidecarState>()
                            .protocol_verified
                            .store(true, Ordering::SeqCst);
                    }
                    Err(error) => {
                        fail_sidecar(&task_handle, &error);
                        return;
                    }
                },
                CommandEvent::Stderr(line) => {
                    eprintln!("desktop PoC sidecar: {}", String::from_utf8_lossy(&line));
                }
                CommandEvent::Error(error) => {
                    fail_sidecar(&task_handle, &format!("sidecar process error: {error}"));
                    return;
                }
                CommandEvent::Terminated(payload) => {
                    let state = task_handle.state::<SidecarState>();
                    if let Ok(mut child) = state.child.lock() {
                        child.take();
                    }
                    if payload.code != Some(0) {
                        eprintln!(
                            "desktop PoC sidecar terminated with failure: {:?}",
                            payload.code
                        );
                        state.failed.store(true, Ordering::SeqCst);
                        state.shutting_down.store(true, Ordering::SeqCst);
                        record_failure_exit_code(1);
                        task_handle.exit(1);
                    } else if state.failed.load(Ordering::SeqCst) {
                        task_handle.exit(1);
                    } else if state.shutting_down.load(Ordering::SeqCst) {
                        task_handle.exit(0);
                    } else {
                        eprintln!(
                            "desktop PoC sidecar terminated unexpectedly: {:?}",
                            payload.code
                        );
                        state.failed.store(true, Ordering::SeqCst);
                        state.shutting_down.store(true, Ordering::SeqCst);
                        record_failure_exit_code(1);
                        task_handle.exit(1);
                    }
                    return;
                }
                _ => {}
            }
        }
    });

    Ok(())
}

fn parse_ready(line: &[u8]) -> Result<tauri::Url, String> {
    let message: ReadyMessage = serde_json::from_slice(line)
        .map_err(|error| format!("invalid READY handshake: {error}"))?;
    if message.kind != "READY" {
        return Err(format!("unexpected sidecar message type: {}", message.kind));
    }
    if message.protocol_version != READY_PROTOCOL_VERSION {
        return Err(format!(
            "unsupported READY protocol version: {}",
            message.protocol_version
        ));
    }
    if message.app_version.trim().is_empty() {
        return Err("READY app version must not be empty".to_string());
    }

    let url: tauri::Url = message
        .url
        .parse()
        .map_err(|error| format!("invalid READY URL: {error}"))?;
    if url.scheme() != "http"
        || url.host_str() != Some("127.0.0.1")
        || url.port().is_none()
        || url.username() != ""
        || url.password().is_some()
        || url.path() != "/"
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("READY URL must be an explicit IPv4 loopback HTTP origin".to_string());
    }
    Ok(url)
}

fn parse_poc_result(line: &[u8]) -> Result<(), String> {
    let message: PocResultMessage = serde_json::from_slice(line)
        .map_err(|error| format!("invalid browser protocol result: {error}"))?;
    if message.kind != "POC_RESULT" {
        return Err(format!("unexpected sidecar message type: {}", message.kind));
    }
    if !message.rest || !message.sse || !message.websocket || !message.external_navigation_blocked {
        return Err(format!(
            "browser protocol verification failed: REST={} SSE={} WebSocket={} ExternalNavigationBlocked={}",
            message.rest, message.sse, message.websocket, message.external_navigation_blocked
        ));
    }
    Ok(())
}

fn navigation_allowed(handle: &AppHandle, url: &tauri::Url) -> bool {
    let state = handle.state::<SidecarState>();
    let allowed_origin = state.allowed_origin.lock().ok();
    is_allowed_url(
        url,
        allowed_origin
            .as_deref()
            .and_then(|origin| origin.as_deref()),
    )
}

fn is_allowed_url(url: &tauri::Url, allowed_origin: Option<&str>) -> bool {
    let app_asset = (url.scheme() == "tauri" && url.host_str() == Some("localhost"))
        || ((url.scheme() == "http" || url.scheme() == "https")
            && url.host_str() == Some("tauri.localhost"));
    app_asset
        || allowed_origin
            .map(|origin| url.origin().ascii_serialization() == origin)
            .unwrap_or(false)
}

fn request_shutdown(handle: &AppHandle) {
    let state = handle.state::<SidecarState>();
    if state.shutting_down.swap(true, Ordering::SeqCst) {
        return;
    }

    let mut has_child = false;
    if let Ok(mut child) = state.child.lock() {
        if let Some(child) = child.as_mut() {
            has_child = true;
            if let Err(error) = child.write(b"SHUTDOWN\n") {
                eprintln!("failed to request sidecar shutdown: {error}");
            }
        }
    }
    if !has_child {
        handle.exit(0);
        return;
    }

    let timeout_handle = handle.clone();
    thread::spawn(move || {
        thread::sleep(FORCE_EXIT_TIMEOUT);
        let state = timeout_handle.state::<SidecarState>();
        let mut forced = false;
        if let Ok(mut child) = state.child.lock() {
            if let Some(child) = child.take() {
                state.failed.store(true, Ordering::SeqCst);
                record_failure_exit_code(2);
                let _ = child.kill();
                forced = true;
            }
        }
        timeout_handle.exit(if forced { 2 } else { 0 });
    });
}

fn fail_sidecar(handle: &AppHandle, message: &str) {
    eprintln!("desktop PoC startup failed: {message}");
    let state = handle.state::<SidecarState>();
    state.failed.store(true, Ordering::SeqCst);
    state.shutting_down.store(true, Ordering::SeqCst);
    record_failure_exit_code(1);
    if let Ok(mut child) = state.child.lock() {
        if let Some(child) = child.take() {
            let _ = child.kill();
        }
    }
    handle.exit(1);
}

fn record_failure_exit_code(code: i32) {
    let _ = DESIRED_EXIT_CODE.compare_exchange(0, code, Ordering::SeqCst, Ordering::SeqCst);
}

#[cfg(debug_assertions)]
fn schedule_automatic_exit(handle: &AppHandle) {
    let Ok(value) = std::env::var("CYBERSTRIKE_DESKTOP_POC_SMOKE_TIMEOUT_MS") else {
        return;
    };
    let Ok(milliseconds) = value.parse::<u64>() else {
        eprintln!("ignoring invalid CYBERSTRIKE_DESKTOP_POC_SMOKE_TIMEOUT_MS");
        return;
    };
    let exit_handle = handle.clone();
    thread::spawn(move || {
        let hold_after_verify = std::env::var("CYBERSTRIKE_DESKTOP_POC_HOLD_AFTER_VERIFY_MS")
            .ok()
            .and_then(|value| value.parse::<u64>().ok())
            .map(Duration::from_millis)
            .unwrap_or_default();
        let timeout = Duration::from_millis(milliseconds);
        let started = Instant::now();
        while started.elapsed() < timeout {
            let state = exit_handle.state::<SidecarState>();
            if state.protocol_verified.load(Ordering::SeqCst)
                && state.download_intercepted.load(Ordering::SeqCst)
            {
                thread::sleep(Duration::from_millis(250));
                thread::sleep(hold_after_verify);
                request_shutdown(&exit_handle);
                return;
            }
            thread::sleep(Duration::from_millis(25));
        }
        fail_sidecar(&exit_handle, "browser protocol verification timed out");
    });
}

#[cfg(test)]
mod tests {
    use super::{is_allowed_url, parse_poc_result, parse_ready};

    #[test]
    fn accepts_versioned_ipv4_loopback_ready() {
        let url = parse_ready(
            br#"{"type":"READY","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"d1-poc"}"#,
        )
        .expect("valid READY message");
        assert_eq!(url.as_str(), "http://127.0.0.1:43123/");
    }

    #[test]
    fn rejects_non_loopback_ready_url() {
        let error = parse_ready(
            br#"{"type":"READY","protocol_version":1,"url":"http://localhost:43123/","app_version":"d1-poc"}"#,
        )
        .expect_err("localhost must not pass the exact IPv4 loopback gate");
        assert!(error.contains("IPv4 loopback"));
    }

    #[test]
    fn rejects_incompatible_protocol() {
        let error = parse_ready(
            br#"{"type":"READY","protocol_version":2,"url":"http://127.0.0.1:43123/","app_version":"d1-poc"}"#,
        )
        .expect_err("unknown protocol version must fail closed");
        assert!(error.contains("protocol version"));
    }

    #[test]
    fn accepts_complete_browser_protocol_result() {
        parse_poc_result(
            br#"{"type":"POC_RESULT","rest":true,"sse":true,"websocket":true,"external_navigation_blocked":true}"#,
        )
        .expect("all browser protocols passed");
    }

    #[test]
    fn rejects_failed_browser_protocol_result() {
        let error = parse_poc_result(
            br#"{"type":"POC_RESULT","rest":true,"sse":false,"websocket":true,"external_navigation_blocked":true}"#,
        )
        .expect_err("failed browser protocol must fail closed");
        assert!(error.contains("SSE=false"));
    }

    #[test]
    fn navigation_policy_allows_only_app_assets_and_current_sidecar_origin() {
        let origin = "http://127.0.0.1:43123";
        assert!(is_allowed_url(
            &"tauri://localhost/index.html".parse().expect("app URL"),
            None
        ));
        assert!(is_allowed_url(
            &"http://127.0.0.1:43123/api/poc/ping"
                .parse()
                .expect("sidecar URL"),
            Some(origin)
        ));
        assert!(!is_allowed_url(
            &"https://example.invalid/desktop-poc"
                .parse()
                .expect("external URL"),
            Some(origin)
        ));
    }
}
