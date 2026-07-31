use serde::{Deserialize, Serialize};
use std::{
    collections::HashSet,
    ffi::OsString,
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicI32, Ordering},
        Mutex,
    },
    thread,
    time::{Duration, Instant},
};
use tauri::{
    webview::{DownloadEvent, NewWindowResponse},
    AppHandle, Manager, RunEvent, WebviewUrl, WebviewWindow, WebviewWindowBuilder,
};
use tauri_plugin_shell::{process::CommandChild, process::CommandEvent, ShellExt};

const SIDECAR_NAME: &str = "cyberstrike-core";
const DESKTOP_PROTOCOL_VERSION: u32 = 1;
const FORCE_EXIT_TIMEOUT: Duration = Duration::from_secs(5);
const SMOKE_BOOTSTRAP_PASSWORD: &str = "desktop-smoke-password";
static DESIRED_EXIT_CODE: AtomicI32 = AtomicI32::new(0);

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
enum StartupPhase {
    #[default]
    Starting,
    CredentialMigrationRequired,
    MigratingCredentials,
    BootstrapRequired,
    Bootstrapping,
    Ready,
    ShuttingDown,
    Failed,
}

#[derive(Default)]
struct SidecarState {
    child: Mutex<Option<CommandChild>>,
    allowed_origin: Mutex<Option<String>>,
    credential_paths: Mutex<Vec<String>>,
    phase: Mutex<StartupPhase>,
}

#[derive(Debug, Deserialize)]
struct HandshakeMessage {
    #[serde(rename = "type")]
    kind: String,
    protocol_version: u32,
    url: Option<String>,
    app_version: String,
    credential_paths: Option<Vec<String>>,
}

#[derive(Debug, Eq, PartialEq)]
enum Handshake {
    CredentialMigrationRequired(Vec<String>),
    BootstrapRequired,
    Ready(tauri::Url),
}

#[derive(Serialize)]
struct BootstrapCommand<'a> {
    #[serde(rename = "type")]
    kind: &'static str,
    protocol_version: u32,
    password: &'a str,
}

#[derive(Serialize)]
struct LifecycleCommand {
    #[serde(rename = "type")]
    kind: &'static str,
    protocol_version: u32,
}

#[derive(Debug)]
struct DesktopPaths {
    data_dir: PathBuf,
    config_dir: PathBuf,
    cache_dir: PathBuf,
    log_dir: PathBuf,
    temp_dir: PathBuf,
    resource_dir: PathBuf,
}

pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            eprintln!("desktop existing instance focused");
            focus_active_window(app);
        }))
        .plugin(tauri_plugin_shell::init())
        .manage(SidecarState::default())
        .invoke_handler(tauri::generate_handler![
            get_credential_migration_paths,
            confirm_credential_migration,
            cancel_credential_migration,
            submit_bootstrap_password
        ])
        .setup(|app| {
            let navigation_handle = app.handle().clone();
            WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("CyberStrikeAI Desktop")
                .inner_size(1120.0, 760.0)
                .min_inner_size(800.0, 560.0)
                .center()
                .on_navigation(move |url| navigation_allowed(&navigation_handle, url))
                .on_new_window(|_url, _features| NewWindowResponse::Deny)
                .on_download(|_webview, event| !matches!(event, DownloadEvent::Requested { .. }))
                .build()?;
            start_sidecar(app.handle())?;
            #[cfg(debug_assertions)]
            schedule_automatic_exit(app.handle());
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build CyberStrikeAI desktop");

    let exit_code = app.run_return(|handle, event| {
        if let RunEvent::ExitRequested { api, .. } = event {
            let terminal = handle
                .state::<SidecarState>()
                .phase
                .lock()
                .map(|phase| matches!(*phase, StartupPhase::ShuttingDown | StartupPhase::Failed))
                .unwrap_or(true);
            if terminal {
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

#[tauri::command]
fn submit_bootstrap_password(window: WebviewWindow, mut password: String) -> Result<(), String> {
    if window.label() != "bootstrap" {
        clear_string(&mut password);
        return Err("bootstrap command is not available to this window".to_string());
    }
    send_bootstrap_password(window.app_handle(), &mut password)
}

#[tauri::command]
fn get_credential_migration_paths(window: WebviewWindow) -> Result<Vec<String>, String> {
    if window.label() != "credential-migration" {
        return Err("credential migration is not available to this window".to_string());
    }
    let state = window.state::<SidecarState>();
    let phase = state
        .phase
        .lock()
        .map_err(|_| "credential migration state is unavailable".to_string())?;
    if *phase != StartupPhase::CredentialMigrationRequired {
        return Err("credential migration is not currently requested".to_string());
    }
    state
        .credential_paths
        .lock()
        .map(|paths| paths.clone())
        .map_err(|_| "credential migration fields are unavailable".to_string())
}

#[tauri::command]
fn confirm_credential_migration(window: WebviewWindow) -> Result<(), String> {
    if window.label() != "credential-migration" {
        return Err("credential migration is not available to this window".to_string());
    }
    send_credential_migration_confirmation(window.app_handle())
}

#[tauri::command]
fn cancel_credential_migration(window: WebviewWindow) -> Result<(), String> {
    if window.label() != "credential-migration" {
        return Err("credential migration is not available to this window".to_string());
    }
    request_shutdown(window.app_handle());
    Ok(())
}

fn start_sidecar(handle: &AppHandle) -> Result<(), Box<dyn std::error::Error>> {
    let paths = resolve_desktop_paths(handle)?;
    let app_version = handle.package_info().version.to_string();
    let arguments = sidecar_arguments(&paths, &app_version);
    let (mut events, child) = handle
        .shell()
        .sidecar(SIDECAR_NAME)?
        .args(arguments)
        .spawn()?;
    handle
        .state::<SidecarState>()
        .child
        .lock()
        .map_err(|_| "sidecar state lock poisoned")?
        .replace(child);

    let task_handle = handle.clone();
    tauri::async_runtime::spawn(async move {
        while let Some(event) = events.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    let handshake = match parse_handshake(&line, &app_version) {
                        Ok(handshake) => handshake,
                        Err(error) => {
                            fail_sidecar(&task_handle, &error);
                            return;
                        }
                    };
                    let action = {
                        let state = task_handle.state::<SidecarState>();
                        let Ok(mut phase) = state.phase.lock() else {
                            fail_sidecar(&task_handle, "startup phase lock poisoned");
                            return;
                        };
                        match apply_handshake(&mut phase, handshake) {
                            Ok(action) => action,
                            Err(error) => {
                                drop(phase);
                                fail_sidecar(&task_handle, &error);
                                return;
                            }
                        }
                    };
                    match action {
                        Handshake::CredentialMigrationRequired(paths) => {
                            eprintln!("desktop core credential migration required");
                            if let Err(error) =
                                show_credential_migration_window(&task_handle, paths)
                            {
                                fail_sidecar(&task_handle, &error);
                                return;
                            }
                            #[cfg(debug_assertions)]
                            if smoke_enabled() {
                                if let Err(error) =
                                    send_credential_migration_confirmation(&task_handle)
                                {
                                    fail_sidecar(&task_handle, &error);
                                    return;
                                }
                            }
                        }
                        Handshake::BootstrapRequired => {
                            eprintln!("desktop core bootstrap required");
                            if let Err(error) = show_bootstrap_window(&task_handle) {
                                fail_sidecar(&task_handle, &error);
                                return;
                            }
                            #[cfg(debug_assertions)]
                            if smoke_enabled() {
                                let mut password = SMOKE_BOOTSTRAP_PASSWORD.to_string();
                                if let Err(error) =
                                    send_bootstrap_password(&task_handle, &mut password)
                                {
                                    fail_sidecar(&task_handle, &error);
                                    return;
                                }
                            }
                        }
                        Handshake::Ready(url) => {
                            if let Err(error) = show_main_window(&task_handle, url) {
                                fail_sidecar(&task_handle, &error);
                                return;
                            }
                            eprintln!("desktop core ready");
                        }
                    }
                }
                CommandEvent::Stderr(line) => {
                    eprintln!("desktop core: {}", String::from_utf8_lossy(&line));
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
                    let phase = state
                        .phase
                        .lock()
                        .map(|phase| *phase)
                        .unwrap_or(StartupPhase::Failed);
                    if payload.code == Some(0) && phase == StartupPhase::ShuttingDown {
                        task_handle.exit(0);
                    } else {
                        eprintln!("desktop core terminated unexpectedly: {:?}", payload.code);
                        if let Ok(mut phase) = state.phase.lock() {
                            *phase = StartupPhase::Failed;
                        }
                        record_failure_exit_code(1);
                        task_handle.exit(1);
                    }
                    return;
                }
                _ => {}
            }
        }
        fail_sidecar(&task_handle, "sidecar event stream closed unexpectedly");
    });

    Ok(())
}

fn parse_handshake(line: &[u8], expected_app_version: &str) -> Result<Handshake, String> {
    let message: HandshakeMessage = serde_json::from_slice(line)
        .map_err(|error| format!("invalid desktop handshake: {error}"))?;
    if message.protocol_version != DESKTOP_PROTOCOL_VERSION {
        return Err(format!(
            "unsupported desktop protocol version: {}",
            message.protocol_version
        ));
    }
    if message.app_version != expected_app_version {
        return Err(format!(
            "desktop core version {} does not match shell version {}",
            message.app_version, expected_app_version
        ));
    }
    match message.kind.as_str() {
        "CREDENTIAL_MIGRATION_REQUIRED" => {
            if message.url.is_some() {
                return Err("CREDENTIAL_MIGRATION_REQUIRED must not include a URL".to_string());
            }
            let paths = message
                .credential_paths
                .ok_or("CREDENTIAL_MIGRATION_REQUIRED must include credential paths")?;
            if paths.is_empty() || paths.iter().any(|path| path.trim().is_empty()) {
                return Err(
                    "CREDENTIAL_MIGRATION_REQUIRED must include non-empty credential paths"
                        .to_string(),
                );
            }
            let unique = paths.iter().collect::<HashSet<_>>();
            if unique.len() != paths.len() {
                return Err(
                    "CREDENTIAL_MIGRATION_REQUIRED must not include duplicate credential paths"
                        .to_string(),
                );
            }
            Ok(Handshake::CredentialMigrationRequired(paths))
        }
        "BOOTSTRAP_REQUIRED" => {
            if message.url.is_some() {
                return Err("BOOTSTRAP_REQUIRED must not include a URL".to_string());
            }
            if message
                .credential_paths
                .as_ref()
                .is_some_and(|paths| !paths.is_empty())
            {
                return Err("BOOTSTRAP_REQUIRED must not include credential paths".to_string());
            }
            Ok(Handshake::BootstrapRequired)
        }
        "READY" => {
            if message
                .credential_paths
                .as_ref()
                .is_some_and(|paths| !paths.is_empty())
            {
                return Err("READY must not include credential paths".to_string());
            }
            let raw_url = message.url.ok_or("READY must include a URL")?;
            let url: tauri::Url = raw_url
                .parse()
                .map_err(|error| format!("invalid READY URL: {error}"))?;
            if !is_exact_loopback_origin(&url) {
                return Err("READY URL must be an explicit IPv4 loopback HTTP origin".to_string());
            }
            Ok(Handshake::Ready(url))
        }
        _ => Err(format!(
            "unexpected desktop handshake type: {}",
            message.kind
        )),
    }
}

fn apply_handshake(phase: &mut StartupPhase, handshake: Handshake) -> Result<Handshake, String> {
    match (*phase, &handshake) {
        (StartupPhase::Starting, Handshake::CredentialMigrationRequired(_)) => {
            *phase = StartupPhase::CredentialMigrationRequired;
        }
        (StartupPhase::Starting, Handshake::BootstrapRequired) => {
            *phase = StartupPhase::BootstrapRequired;
        }
        (StartupPhase::MigratingCredentials, Handshake::BootstrapRequired) => {
            *phase = StartupPhase::BootstrapRequired;
        }
        (
            StartupPhase::Starting
            | StartupPhase::MigratingCredentials
            | StartupPhase::Bootstrapping,
            Handshake::Ready(_),
        ) => {
            *phase = StartupPhase::Ready;
        }
        _ => {
            return Err(format!(
                "unexpected desktop handshake {:?} while in phase {:?}",
                handshake, phase
            ));
        }
    }
    Ok(handshake)
}

fn send_credential_migration_confirmation(handle: &AppHandle) -> Result<(), String> {
    let state = handle.state::<SidecarState>();
    let mut phase = state
        .phase
        .lock()
        .map_err(|_| "credential migration state is unavailable".to_string())?;
    if *phase != StartupPhase::CredentialMigrationRequired {
        return Err("credential migration is not currently requested".to_string());
    }

    let mut command = serde_json::to_vec(&LifecycleCommand {
        kind: "MIGRATE_CREDENTIALS",
        protocol_version: DESKTOP_PROTOCOL_VERSION,
    })
    .map_err(|_| "failed to encode credential migration command".to_string())?;
    command.push(b'\n');
    let write_result = state
        .child
        .lock()
        .map_err(|_| "desktop core is unavailable".to_string())?
        .as_mut()
        .ok_or_else(|| "desktop core is unavailable".to_string())?
        .write(&command)
        .map_err(|_| "failed to confirm credential migration".to_string());
    command.fill(0);
    write_result?;
    *phase = StartupPhase::MigratingCredentials;
    Ok(())
}

fn send_bootstrap_password(handle: &AppHandle, password: &mut String) -> Result<(), String> {
    if password.trim().len() < 8 {
        clear_string(password);
        return Err("password must contain at least 8 characters".to_string());
    }

    let state = handle.state::<SidecarState>();
    let mut phase = state
        .phase
        .lock()
        .map_err(|_| "bootstrap state is unavailable".to_string())?;
    if *phase != StartupPhase::BootstrapRequired {
        clear_string(password);
        return Err("bootstrap password is not currently requested".to_string());
    }

    let mut command = serde_json::to_vec(&BootstrapCommand {
        kind: "BOOTSTRAP",
        protocol_version: DESKTOP_PROTOCOL_VERSION,
        password,
    })
    .map_err(|_| "failed to encode bootstrap command".to_string())?;
    command.push(b'\n');
    clear_string(password);

    let write_result = state
        .child
        .lock()
        .map_err(|_| "desktop core is unavailable".to_string())?
        .as_mut()
        .ok_or_else(|| "desktop core is unavailable".to_string())?
        .write(&command)
        .map_err(|_| "failed to submit bootstrap password".to_string());
    command.fill(0);
    write_result?;
    *phase = StartupPhase::Bootstrapping;
    Ok(())
}

fn clear_string(value: &mut String) {
    // SAFETY: bytes are overwritten without changing their length or violating UTF-8;
    // the string is cleared immediately afterwards.
    unsafe {
        value.as_mut_vec().fill(0);
    }
    value.clear();
}

fn show_bootstrap_window(handle: &AppHandle) -> Result<(), String> {
    if let Some(main) = handle.get_webview_window("main") {
        main.hide()
            .map_err(|error| format!("hide main window: {error}"))?;
    }
    if let Some(migration) = handle.get_webview_window("credential-migration") {
        migration
            .destroy()
            .map_err(|error| format!("destroy credential migration window: {error}"))?;
    }
    if let Some(window) = handle.get_webview_window("bootstrap") {
        window
            .show()
            .map_err(|error| format!("show bootstrap window: {error}"))?;
        window
            .set_focus()
            .map_err(|error| format!("focus bootstrap window: {error}"))?;
        return Ok(());
    }
    WebviewWindowBuilder::new(
        handle,
        "bootstrap",
        WebviewUrl::App("bootstrap.html".into()),
    )
    .title("Initialize CyberStrikeAI")
    .inner_size(480.0, 500.0)
    .resizable(false)
    .maximizable(false)
    .minimizable(false)
    .closable(false)
    .center()
    .on_navigation(is_app_asset_url)
    .on_new_window(|_url, _features| NewWindowResponse::Deny)
    .on_download(|_webview, event| !matches!(event, DownloadEvent::Requested { .. }))
    .build()
    .map_err(|error| format!("create bootstrap window: {error}"))?;
    Ok(())
}

fn show_credential_migration_window(handle: &AppHandle, paths: Vec<String>) -> Result<(), String> {
    if let Some(main) = handle.get_webview_window("main") {
        main.hide()
            .map_err(|error| format!("hide main window: {error}"))?;
    }
    *handle
        .state::<SidecarState>()
        .credential_paths
        .lock()
        .map_err(|_| "credential migration fields are unavailable".to_string())? = paths;
    if let Some(window) = handle.get_webview_window("credential-migration") {
        window
            .show()
            .map_err(|error| format!("show credential migration window: {error}"))?;
        window
            .set_focus()
            .map_err(|error| format!("focus credential migration window: {error}"))?;
        return Ok(());
    }
    WebviewWindowBuilder::new(
        handle,
        "credential-migration",
        WebviewUrl::App("credential-migration.html".into()),
    )
    .title("Protect CyberStrikeAI credentials")
    .inner_size(520.0, 560.0)
    .resizable(false)
    .maximizable(false)
    .minimizable(false)
    .closable(false)
    .center()
    .on_navigation(is_app_asset_url)
    .on_new_window(|_url, _features| NewWindowResponse::Deny)
    .on_download(|_webview, event| !matches!(event, DownloadEvent::Requested { .. }))
    .build()
    .map_err(|error| format!("create credential migration window: {error}"))?;
    Ok(())
}

fn show_main_window(handle: &AppHandle, url: tauri::Url) -> Result<(), String> {
    let origin = url.origin().ascii_serialization();
    handle
        .state::<SidecarState>()
        .allowed_origin
        .lock()
        .map_err(|_| "allowed origin state lock poisoned".to_string())?
        .replace(origin);
    let window = handle
        .get_webview_window("main")
        .ok_or_else(|| "main window is missing".to_string())?;
    window
        .navigate(url)
        .map_err(|error| format!("navigate to desktop core: {error}"))?;
    if let Some(bootstrap) = handle.get_webview_window("bootstrap") {
        bootstrap
            .destroy()
            .map_err(|error| format!("destroy bootstrap window: {error}"))?;
    }
    if let Some(migration) = handle.get_webview_window("credential-migration") {
        migration
            .destroy()
            .map_err(|error| format!("destroy credential migration window: {error}"))?;
    }
    window
        .show()
        .map_err(|error| format!("show main window: {error}"))?;
    window
        .set_focus()
        .map_err(|error| format!("focus main window: {error}"))?;
    Ok(())
}

fn focus_active_window(handle: &AppHandle) {
    let phase = handle
        .state::<SidecarState>()
        .phase
        .lock()
        .map(|phase| *phase)
        .unwrap_or(StartupPhase::Failed);
    let label = if matches!(
        phase,
        StartupPhase::CredentialMigrationRequired | StartupPhase::MigratingCredentials
    ) {
        "credential-migration"
    } else if matches!(
        phase,
        StartupPhase::BootstrapRequired | StartupPhase::Bootstrapping
    ) {
        "bootstrap"
    } else {
        "main"
    };
    if let Some(window) = handle.get_webview_window(label) {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
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
    is_app_asset_url(url)
        || allowed_origin
            .map(|origin| url.origin().ascii_serialization() == origin)
            .unwrap_or(false)
}

fn is_app_asset_url(url: &tauri::Url) -> bool {
    (url.scheme() == "tauri" && url.host_str() == Some("localhost"))
        || ((url.scheme() == "http" || url.scheme() == "https")
            && url.host_str() == Some("tauri.localhost"))
}

fn is_exact_loopback_origin(url: &tauri::Url) -> bool {
    url.scheme() == "http"
        && url.host_str() == Some("127.0.0.1")
        && url.port().is_some()
        && url.username().is_empty()
        && url.password().is_none()
        && url.path() == "/"
        && url.query().is_none()
        && url.fragment().is_none()
}

fn resolve_desktop_paths(handle: &AppHandle) -> Result<DesktopPaths, Box<dyn std::error::Error>> {
    #[cfg(debug_assertions)]
    if let Ok(root_value) = std::env::var("CYBERSTRIKE_DESKTOP_TEST_ROOT") {
        let root = PathBuf::from(root_value);
        let resource_dir = PathBuf::from(std::env::var("CYBERSTRIKE_DESKTOP_RESOURCE_DIR")?);
        if !root.is_absolute() || !resource_dir.is_absolute() {
            return Err("desktop test paths must be absolute".into());
        }
        return Ok(DesktopPaths {
            data_dir: root.join("data"),
            config_dir: root.join("config"),
            cache_dir: root.join("cache"),
            log_dir: root.join("logs"),
            temp_dir: root.join("temp"),
            resource_dir,
        });
    }

    let resolver = handle.path();
    let cache_dir = resolver.app_cache_dir()?;
    let paths = DesktopPaths {
        data_dir: resolver.app_data_dir()?,
        config_dir: resolver.app_config_dir()?,
        cache_dir,
        log_dir: resolver.app_log_dir()?,
        temp_dir: resolver.temp_dir()?.join("cyberstrikeai-desktop"),
        resource_dir: resolver.resource_dir()?.join("defaults"),
    };
    for path in [
        &paths.data_dir,
        &paths.config_dir,
        &paths.cache_dir,
        &paths.log_dir,
        &paths.temp_dir,
        &paths.resource_dir,
    ] {
        if !path.is_absolute() {
            return Err(format!("desktop path is not absolute: {}", path.display()).into());
        }
    }
    Ok(paths)
}

fn sidecar_arguments(paths: &DesktopPaths, app_version: &str) -> Vec<OsString> {
    let mut arguments = Vec::with_capacity(14);
    push_path_argument(&mut arguments, "--data-dir", &paths.data_dir);
    push_path_argument(&mut arguments, "--config-dir", &paths.config_dir);
    push_path_argument(&mut arguments, "--cache-dir", &paths.cache_dir);
    push_path_argument(&mut arguments, "--log-dir", &paths.log_dir);
    push_path_argument(&mut arguments, "--temp-dir", &paths.temp_dir);
    push_path_argument(&mut arguments, "--resource-dir", &paths.resource_dir);
    arguments.push(OsString::from("--app-version"));
    arguments.push(OsString::from(app_version));
    arguments
}

fn push_path_argument(arguments: &mut Vec<OsString>, name: &str, path: &Path) {
    arguments.push(OsString::from(name));
    arguments.push(path.as_os_str().to_os_string());
}

fn request_shutdown(handle: &AppHandle) {
    let state = handle.state::<SidecarState>();
    let Ok(mut phase) = state.phase.lock() else {
        record_failure_exit_code(2);
        handle.exit(2);
        return;
    };
    if matches!(*phase, StartupPhase::ShuttingDown | StartupPhase::Failed) {
        return;
    }
    *phase = StartupPhase::ShuttingDown;
    drop(phase);

    let mut has_child = false;
    if let Ok(mut child) = state.child.lock() {
        if let Some(child) = child.as_mut() {
            has_child = true;
            if child
                .write(b"{\"type\":\"SHUTDOWN\",\"protocol_version\":1}\n")
                .is_err()
            {
                eprintln!("failed to request desktop core shutdown");
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
                record_failure_exit_code(2);
                let _ = child.kill();
                forced = true;
            }
        }
        timeout_handle.exit(if forced { 2 } else { 0 });
    });
}

fn fail_sidecar(handle: &AppHandle, message: &str) {
    eprintln!("desktop core startup failed: {message}");
    let state = handle.state::<SidecarState>();
    if let Ok(mut phase) = state.phase.lock() {
        *phase = StartupPhase::Failed;
    }
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
fn smoke_enabled() -> bool {
    std::env::var("CYBERSTRIKE_DESKTOP_SMOKE_TIMEOUT_MS").is_ok()
}

#[cfg(debug_assertions)]
fn schedule_automatic_exit(handle: &AppHandle) {
    let Ok(value) = std::env::var("CYBERSTRIKE_DESKTOP_SMOKE_TIMEOUT_MS") else {
        return;
    };
    let Ok(milliseconds) = value.parse::<u64>() else {
        eprintln!("ignoring invalid CYBERSTRIKE_DESKTOP_SMOKE_TIMEOUT_MS");
        return;
    };
    let exit_handle = handle.clone();
    thread::spawn(move || {
        let hold_after_ready = std::env::var("CYBERSTRIKE_DESKTOP_HOLD_AFTER_READY_MS")
            .ok()
            .and_then(|value| value.parse::<u64>().ok())
            .map(Duration::from_millis)
            .unwrap_or_default();
        let timeout = Duration::from_millis(milliseconds);
        let started = Instant::now();
        while started.elapsed() < timeout {
            let ready = exit_handle
                .state::<SidecarState>()
                .phase
                .lock()
                .map(|phase| *phase == StartupPhase::Ready)
                .unwrap_or(false);
            if ready {
                thread::sleep(Duration::from_millis(500));
                thread::sleep(hold_after_ready);
                request_shutdown(&exit_handle);
                return;
            }
            thread::sleep(Duration::from_millis(25));
        }
        fail_sidecar(&exit_handle, "desktop core readiness timed out");
    });
}

#[cfg(test)]
mod tests {
    use super::{
        apply_handshake, is_allowed_url, parse_handshake, sidecar_arguments, DesktopPaths,
        Handshake, StartupPhase,
    };
    use std::path::PathBuf;

    #[test]
    fn accepts_versioned_migration_bootstrap_and_ready_messages() {
        let migration = parse_handshake(
            br#"{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"0.1.0","credential_paths":["fofa.api_key"]}"#,
            "0.1.0",
        )
        .expect("valid credential migration message");
        assert_eq!(
            migration,
            Handshake::CredentialMigrationRequired(vec!["fofa.api_key".to_string()])
        );

        let bootstrap = parse_handshake(
            br#"{"type":"BOOTSTRAP_REQUIRED","protocol_version":1,"app_version":"0.1.0"}"#,
            "0.1.0",
        )
        .expect("valid bootstrap message");
        assert_eq!(bootstrap, Handshake::BootstrapRequired);

        let ready = parse_handshake(
            br#"{"type":"READY","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"0.1.0"}"#,
            "0.1.0",
        )
        .expect("valid READY message");
        assert!(matches!(ready, Handshake::Ready(_)));
    }

    #[test]
    fn rejects_unsafe_credential_migration_messages() {
        for message in [
            br#"{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"0.1.0"}"#.as_slice(),
            br#"{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"0.1.0","credential_paths":[]}"#.as_slice(),
            br#"{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"0.1.0","credential_paths":[""]}"#.as_slice(),
            br#"{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"0.1.0","credential_paths":["fofa.api_key","fofa.api_key"]}"#.as_slice(),
            br#"{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"0.1.0","credential_paths":["fofa.api_key"]}"#.as_slice(),
        ] {
            assert!(parse_handshake(message, "0.1.0").is_err());
        }
    }

    #[test]
    fn rejects_non_loopback_or_incompatible_ready_messages() {
        let non_loopback = parse_handshake(
            br#"{"type":"READY","protocol_version":1,"url":"http://localhost:43123/","app_version":"0.1.0"}"#,
            "0.1.0",
        )
        .expect_err("localhost must not pass the exact IPv4 loopback gate");
        assert!(non_loopback.contains("IPv4 loopback"));

        let incompatible = parse_handshake(
            br#"{"type":"READY","protocol_version":2,"url":"http://127.0.0.1:43123/","app_version":"0.1.0"}"#,
            "0.1.0",
        )
        .expect_err("unknown protocol version must fail closed");
        assert!(incompatible.contains("protocol version"));

        let version_mismatch = parse_handshake(
            br#"{"type":"READY","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"0.2.0"}"#,
            "0.1.0",
        )
        .expect_err("core and shell versions must match");
        assert!(version_mismatch.contains("does not match"));
    }

    #[test]
    fn startup_state_machine_requires_bootstrap_before_post_bootstrap_ready() {
        let mut phase = StartupPhase::Starting;
        apply_handshake(&mut phase, Handshake::BootstrapRequired).expect("bootstrap transition");
        assert_eq!(phase, StartupPhase::BootstrapRequired);
        assert!(apply_handshake(
            &mut phase,
            Handshake::Ready("http://127.0.0.1:43123/".parse().expect("URL"))
        )
        .is_err());
        phase = StartupPhase::Bootstrapping;
        apply_handshake(
            &mut phase,
            Handshake::Ready("http://127.0.0.1:43123/".parse().expect("URL")),
        )
        .expect("READY transition");
        assert_eq!(phase, StartupPhase::Ready);
    }

    #[test]
    fn startup_state_machine_requires_confirmation_after_migration_notice() {
        let mut phase = StartupPhase::Starting;
        apply_handshake(
            &mut phase,
            Handshake::CredentialMigrationRequired(vec!["fofa.api_key".to_string()]),
        )
        .expect("credential migration transition");
        assert_eq!(phase, StartupPhase::CredentialMigrationRequired);
        assert!(apply_handshake(&mut phase, Handshake::BootstrapRequired).is_err());

        phase = StartupPhase::MigratingCredentials;
        apply_handshake(&mut phase, Handshake::BootstrapRequired)
            .expect("post-migration bootstrap transition");
        assert_eq!(phase, StartupPhase::BootstrapRequired);

        phase = StartupPhase::MigratingCredentials;
        apply_handshake(
            &mut phase,
            Handshake::Ready("http://127.0.0.1:43123/".parse().expect("URL")),
        )
        .expect("post-migration READY transition");
        assert_eq!(phase, StartupPhase::Ready);
    }

    #[test]
    fn navigation_policy_allows_only_app_assets_and_current_core_origin() {
        let origin = "http://127.0.0.1:43123";
        assert!(is_allowed_url(
            &"tauri://localhost/index.html".parse().expect("app URL"),
            None
        ));
        assert!(is_allowed_url(
            &"http://127.0.0.1:43123/api/health"
                .parse()
                .expect("core URL"),
            Some(origin)
        ));
        assert!(!is_allowed_url(
            &"https://example.invalid/desktop"
                .parse()
                .expect("external URL"),
            Some(origin)
        ));
    }

    #[test]
    fn sidecar_arguments_contain_only_paths_and_version() {
        let root = PathBuf::from("/tmp/cyberstrike-desktop-test");
        let paths = DesktopPaths {
            data_dir: root.join("data"),
            config_dir: root.join("config"),
            cache_dir: root.join("cache"),
            log_dir: root.join("logs"),
            temp_dir: root.join("temp"),
            resource_dir: root.join("resources"),
        };
        let arguments = sidecar_arguments(&paths, "0.1.0");
        let rendered = arguments
            .iter()
            .map(|value| value.to_string_lossy())
            .collect::<Vec<_>>()
            .join(" ");
        assert!(rendered.contains("--data-dir"));
        assert!(rendered.contains("--resource-dir"));
        assert!(rendered.contains("--app-version 0.1.0"));
        assert!(!rendered.to_lowercase().contains("password"));
    }
}
