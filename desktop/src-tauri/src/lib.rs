use serde::{Deserialize, Serialize};
#[cfg(debug_assertions)]
use std::time::Instant;
use std::{
    collections::HashSet,
    ffi::OsString,
    fs,
    io::Write,
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering},
        Mutex,
    },
    thread,
    time::Duration,
};
use tauri::{
    webview::{DownloadEvent, NewWindowResponse},
    AppHandle, Manager, RunEvent, WebviewUrl, WebviewWindow, WebviewWindowBuilder, WindowEvent,
};
use tauri_plugin_shell::{process::CommandChild, process::CommandEvent, ShellExt};

mod maintenance;
mod plugin_integration;

const SIDECAR_NAME: &str = env!("CYBERSTRIKE_DESKTOP_CORE_BINARY_BASENAME");
const DESKTOP_PROTOCOL_VERSION: u32 = 1;
const FORCE_EXIT_TIMEOUT: Duration = Duration::from_secs(5);
#[cfg(debug_assertions)]
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
    MaintenanceStopping,
    Maintenance,
    ShuttingDown,
    Failed,
}

#[derive(Default)]
struct SidecarState {
    child: Mutex<Option<CommandChild>>,
    allowed_origin: Mutex<Option<String>>,
    credential_paths: Mutex<Vec<String>>,
    desktop_paths: Mutex<Option<DesktopPaths>>,
    startup_failure: Mutex<Option<StartupFailure>>,
    window_placement: Mutex<Option<WindowPlacement>>,
    generation: AtomicU64,
    phase: Mutex<StartupPhase>,
    maintenance_waiter: Mutex<Option<std::sync::mpsc::Sender<Result<(), String>>>>,
    maintenance_holds_main: Mutex<bool>,
    maintenance_active: AtomicBool,
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

#[derive(Clone, Debug)]
struct DesktopPaths {
    data_dir: PathBuf,
    config_dir: PathBuf,
    cache_dir: PathBuf,
    log_dir: PathBuf,
    temp_dir: PathBuf,
    resource_dir: PathBuf,
    python_runtime_dir: Option<PathBuf>,
}

#[derive(Clone, Copy, Debug, Serialize)]
struct StartupFailure {
    code: &'static str,
    title: &'static str,
    message: &'static str,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize)]
struct WindowPlacement {
    width: u32,
    height: u32,
    maximized: bool,
}

impl Default for WindowPlacement {
    fn default() -> Self {
        Self {
            width: 1120,
            height: 760,
            maximized: false,
        }
    }
}

pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            eprintln!("desktop existing instance focused");
            focus_active_window(app);
        }))
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .manage(SidecarState::default())
        .manage(plugin_integration::PluginIntegrationState::default())
        .invoke_handler(tauri::generate_handler![
            get_credential_migration_paths,
            confirm_credential_migration,
            cancel_credential_migration,
            submit_bootstrap_password,
            get_startup_failure,
            retry_startup,
            exit_after_startup_failure,
            open_desktop_directory,
            maintenance::open_data_maintenance,
            maintenance::get_data_maintenance_state,
            maintenance::choose_and_prepare_legacy_import,
            maintenance::confirm_legacy_import,
            maintenance::cancel_legacy_import,
            maintenance::restore_desktop_backup,
            maintenance::delete_desktop_backup,
            maintenance::close_data_maintenance,
            plugin_integration::get_plugin_integration_status,
            plugin_integration::set_plugin_integration_enabled
        ])
        .setup(|app| {
            let navigation_handle = app.handle().clone();
            let paths = resolve_desktop_paths(app.handle());
            let placement = paths
                .as_ref()
                .ok()
                .and_then(|paths| load_window_placement(&paths.config_dir).ok().flatten())
                .unwrap_or_default();
            let main = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("CyberStrikeAI Desktop")
                .inner_size(placement.width as f64, placement.height as f64)
                .min_inner_size(800.0, 560.0)
                .maximized(placement.maximized)
                .center()
                .on_navigation(move |url| navigation_allowed(&navigation_handle, url))
                .on_new_window(|_url, _features| NewWindowResponse::Deny)
                .on_download(|_webview, event| !matches!(event, DownloadEvent::Requested { .. }))
                .build()?;
            app.state::<SidecarState>()
                .window_placement
                .lock()
                .map_err(|_| "window placement state lock poisoned")?
                .replace(placement);
            match paths {
                Ok(paths) => {
                    app.state::<SidecarState>()
                        .desktop_paths
                        .lock()
                        .map_err(|_| "desktop paths state lock poisoned")?
                        .replace(paths.clone());
                    plugin_integration::initialize(app.handle(), &paths);
                    if let Err(error) = start_sidecar(app.handle(), paths) {
                        fail_sidecar(app.handle(), &error.to_string());
                    }
                }
                Err(error) => fail_sidecar(app.handle(), &error.to_string()),
            }
            drop(main);
            #[cfg(debug_assertions)]
            schedule_automatic_exit(app.handle());
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build CyberStrikeAI desktop");

    let exit_code = app.run_return(|handle, event| match event {
        RunEvent::WindowEvent { label, event, .. } if label == "main" => {
            update_window_placement(handle, &event);
        }
        RunEvent::ExitRequested { api, .. } => {
            if let Err(error) = save_window_placement(handle) {
                eprintln!("failed to save desktop window placement: {error}");
            }
            let state = handle.state::<SidecarState>();
            let phase = state
                .phase
                .lock()
                .map(|phase| *phase)
                .unwrap_or(StartupPhase::Failed);
            if state.maintenance_active.load(Ordering::SeqCst)
                || matches!(
                    phase,
                    StartupPhase::MaintenanceStopping | StartupPhase::Maintenance
                )
            {
                api.prevent_exit();
            } else if !matches!(phase, StartupPhase::ShuttingDown | StartupPhase::Failed) {
                api.prevent_exit();
                request_shutdown(handle);
            }
        }
        _ => {}
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

#[tauri::command]
fn get_startup_failure(window: WebviewWindow) -> Result<StartupFailure, String> {
    if window.label() != "startup-error" {
        return Err("startup failure details are not available to this window".to_string());
    }
    let state = window.state::<SidecarState>();
    if *state
        .phase
        .lock()
        .map_err(|_| "startup failure state is unavailable".to_string())?
        != StartupPhase::Failed
    {
        return Err("desktop startup has not failed".to_string());
    }
    let failure = state
        .startup_failure
        .lock()
        .map_err(|_| "startup failure details are unavailable".to_string())?
        .clone()
        .ok_or_else(|| "startup failure details are unavailable".to_string())?;
    Ok(failure)
}

#[tauri::command]
fn retry_startup(window: WebviewWindow) -> Result<(), String> {
    if window.label() != "startup-error" {
        return Err("startup retry is not available to this window".to_string());
    }
    let handle = window.app_handle();
    let state = handle.state::<SidecarState>();
    let mut phase = state
        .phase
        .lock()
        .map_err(|_| "startup retry state is unavailable".to_string())?;
    if *phase != StartupPhase::Failed {
        return Err("desktop startup is not currently retryable".to_string());
    }
    let paths = resolve_desktop_paths(handle).map_err(|_| "desktop paths are unavailable")?;
    state
        .desktop_paths
        .lock()
        .map_err(|_| "desktop paths are unavailable".to_string())?
        .replace(paths.clone());
    *phase = StartupPhase::Starting;
    drop(phase);
    DESIRED_EXIT_CODE.store(0, Ordering::SeqCst);
    if let Ok(mut failure) = state.startup_failure.lock() {
        failure.take();
    }
    if let Err(error) = start_sidecar(handle, paths) {
        fail_sidecar(handle, &error.to_string());
        return Err("desktop core could not be restarted".to_string());
    }
    window
        .hide()
        .map_err(|error| format!("hide startup error window: {error}"))?;
    Ok(())
}

#[tauri::command]
fn exit_after_startup_failure(window: WebviewWindow) -> Result<(), String> {
    if window.label() != "startup-error" {
        return Err("startup failure exit is not available to this window".to_string());
    }
    record_failure_exit_code(1);
    window.app_handle().exit(1);
    Ok(())
}

#[tauri::command]
fn open_desktop_directory(window: WebviewWindow, directory: String) -> Result<(), String> {
    if window.label() != "startup-error" && window.label() != "main" {
        return Err("desktop directories are not available to this window".to_string());
    }
    let state = window.state::<SidecarState>();
    let paths = state
        .desktop_paths
        .lock()
        .map_err(|_| "desktop paths are unavailable".to_string())?
        .clone()
        .ok_or_else(|| "desktop paths are unavailable".to_string())?;
    let path = match directory.trim() {
        "logs" => paths.log_dir,
        "data" => paths.data_dir,
        _ => return Err("unsupported desktop directory".to_string()),
    };
    fs::create_dir_all(&path).map_err(|_| "desktop directory is unavailable".to_string())?;
    #[allow(deprecated)]
    window
        .shell()
        .open(path.to_string_lossy(), None)
        .map_err(|_| "desktop directory could not be opened".to_string())
}

fn start_sidecar(
    handle: &AppHandle,
    paths: DesktopPaths,
) -> Result<(), Box<dyn std::error::Error>> {
    let generation = handle
        .state::<SidecarState>()
        .generation
        .fetch_add(1, Ordering::SeqCst)
        + 1;
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
            if task_handle
                .state::<SidecarState>()
                .generation
                .load(Ordering::SeqCst)
                != generation
            {
                return;
            }
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
                            if let Ok(mut failure) =
                                task_handle.state::<SidecarState>().startup_failure.lock()
                            {
                                failure.take();
                            }
                            let origin = url.origin().ascii_serialization();
                            if let Err(error) = show_main_window(&task_handle, url) {
                                fail_sidecar(&task_handle, &error);
                                return;
                            }
                            plugin_integration::core_ready(&task_handle, &origin, &app_version);
                            eprintln!("desktop core ready");
                        }
                    }
                }
                CommandEvent::Stderr(line) => {
                    let message = String::from_utf8_lossy(&line);
                    eprintln!("desktop core: {message}");
                    let failure = classify_startup_failure(&message);
                    if failure.code != "core_startup" {
                        if let Ok(mut recorded) =
                            task_handle.state::<SidecarState>().startup_failure.lock()
                        {
                            recorded.replace(failure);
                        }
                    }
                }
                CommandEvent::Error(error) => {
                    let maintenance_stopping = task_handle
                        .state::<SidecarState>()
                        .phase
                        .lock()
                        .map(|phase| *phase == StartupPhase::MaintenanceStopping)
                        .unwrap_or(false);
                    if maintenance_stopping {
                        if let Ok(mut child) = task_handle.state::<SidecarState>().child.lock() {
                            if let Some(child) = child.take() {
                                let _ = child.kill();
                            }
                        }
                        maintenance::finish_core_stop(
                            &task_handle,
                            Err("the local core stopped unexpectedly".to_string()),
                        );
                        return;
                    }
                    fail_sidecar(&task_handle, &format!("sidecar process error: {error}"));
                    return;
                }
                CommandEvent::Terminated(payload) => {
                    plugin_integration::core_unavailable(&task_handle);
                    let state = task_handle.state::<SidecarState>();
                    if let Ok(mut child) = state.child.lock() {
                        child.take();
                    }
                    let phase = state
                        .phase
                        .lock()
                        .map(|phase| *phase)
                        .unwrap_or(StartupPhase::Failed);
                    if phase == StartupPhase::MaintenanceStopping {
                        let result = if payload.code == Some(0) {
                            Ok(())
                        } else {
                            Err("the local core did not stop cleanly".to_string())
                        };
                        maintenance::finish_core_stop(&task_handle, result);
                    } else if payload.code == Some(0) && phase == StartupPhase::ShuttingDown {
                        task_handle.exit(0);
                    } else {
                        eprintln!("desktop core terminated unexpectedly: {:?}", payload.code);
                        fail_sidecar(&task_handle, "desktop core terminated unexpectedly");
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
    if let Ok(mut hold) = handle.state::<SidecarState>().maintenance_holds_main.lock() {
        *hold = false;
    }
    if let Some(main) = handle.get_webview_window("main") {
        main.hide()
            .map_err(|error| format!("hide main window: {error}"))?;
    }
    if let Some(migration) = handle.get_webview_window("credential-migration") {
        migration
            .destroy()
            .map_err(|error| format!("destroy credential migration window: {error}"))?;
    }
    if let Some(startup_error) = handle.get_webview_window("startup-error") {
        startup_error
            .destroy()
            .map_err(|error| format!("destroy startup error window: {error}"))?;
    }
    if let Some(maintenance) = handle.get_webview_window("data-maintenance") {
        maintenance
            .destroy()
            .map_err(|error| format!("destroy data maintenance window: {error}"))?;
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
    if let Ok(mut hold) = handle.state::<SidecarState>().maintenance_holds_main.lock() {
        *hold = false;
    }
    if let Some(main) = handle.get_webview_window("main") {
        main.hide()
            .map_err(|error| format!("hide main window: {error}"))?;
    }
    if let Some(startup_error) = handle.get_webview_window("startup-error") {
        startup_error
            .destroy()
            .map_err(|error| format!("destroy startup error window: {error}"))?;
    }
    if let Some(maintenance) = handle.get_webview_window("data-maintenance") {
        maintenance
            .destroy()
            .map_err(|error| format!("destroy data maintenance window: {error}"))?;
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

fn show_startup_error_window(handle: &AppHandle) -> Result<(), String> {
    for label in [
        "main",
        "bootstrap",
        "credential-migration",
        "data-maintenance",
    ] {
        if let Some(window) = handle.get_webview_window(label) {
            window
                .hide()
                .map_err(|error| format!("hide {label} window: {error}"))?;
        }
    }
    if let Some(window) = handle.get_webview_window("startup-error") {
        window
            .show()
            .map_err(|error| format!("show startup error window: {error}"))?;
        window
            .set_focus()
            .map_err(|error| format!("focus startup error window: {error}"))?;
        return Ok(());
    }
    WebviewWindowBuilder::new(
        handle,
        "startup-error",
        WebviewUrl::App("startup-error.html".into()),
    )
    .title("CyberStrikeAI could not start")
    .inner_size(520.0, 570.0)
    .resizable(false)
    .maximizable(false)
    .minimizable(true)
    .center()
    .on_navigation(is_app_asset_url)
    .on_new_window(|_url, _features| NewWindowResponse::Deny)
    .on_download(|_webview, event| !matches!(event, DownloadEvent::Requested { .. }))
    .build()
    .map_err(|error| format!("create startup error window: {error}"))?;
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
    if let Some(startup_error) = handle.get_webview_window("startup-error") {
        startup_error
            .destroy()
            .map_err(|error| format!("destroy startup error window: {error}"))?;
    }
    if maintenance::maintenance_holds_main(handle) {
        if let Some(maintenance) = handle.get_webview_window("data-maintenance") {
            window
                .hide()
                .map_err(|error| format!("hide main window: {error}"))?;
            maintenance
                .show()
                .map_err(|error| format!("show data maintenance window: {error}"))?;
            maintenance
                .set_focus()
                .map_err(|error| format!("focus data maintenance window: {error}"))?;
            return Ok(());
        }
        if let Ok(mut hold) = handle.state::<SidecarState>().maintenance_holds_main.lock() {
            *hold = false;
        }
    }
    if let Some(maintenance) = handle.get_webview_window("data-maintenance") {
        maintenance
            .destroy()
            .map_err(|error| format!("destroy data maintenance window: {error}"))?;
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
    let label = if phase == StartupPhase::Failed {
        "startup-error"
    } else if maintenance::maintenance_holds_main(handle)
        || matches!(
            phase,
            StartupPhase::MaintenanceStopping | StartupPhase::Maintenance
        )
    {
        "data-maintenance"
    } else if matches!(
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
        let python_runtime_dir = std::env::var_os("CYBERSTRIKE_DESKTOP_PYTHON_RUNTIME_DIR")
            .map(PathBuf::from);
        if !root.is_absolute()
            || !resource_dir.is_absolute()
            || python_runtime_dir
                .as_ref()
                .is_some_and(|path| !path.is_absolute())
        {
            return Err("desktop test paths must be absolute".into());
        }
        return Ok(DesktopPaths {
            data_dir: root.join("data"),
            config_dir: root.join("config"),
            cache_dir: root.join("cache"),
            log_dir: root.join("logs"),
            temp_dir: root.join("temp"),
            resource_dir,
            python_runtime_dir,
        });
    }

    let resolver = handle.path();
    let cache_dir = resolver.app_cache_dir()?;
    let resource_root = resolver.resource_dir()?;
    let paths = DesktopPaths {
        data_dir: resolver.app_data_dir()?,
        config_dir: resolver.app_config_dir()?,
        cache_dir,
        log_dir: resolver.app_log_dir()?,
        temp_dir: resolver.temp_dir()?.join("cyberstrikeai-desktop"),
        resource_dir: resource_root.join("defaults"),
        python_runtime_dir: cfg!(target_os = "windows")
            .then(|| resource_root.join("runtime").join("python")),
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
    if paths
        .python_runtime_dir
        .as_ref()
        .is_some_and(|path| !path.is_absolute())
    {
        return Err("desktop Python runtime path is not absolute".into());
    }
    Ok(paths)
}

fn load_window_placement(config_dir: &Path) -> Result<Option<WindowPlacement>, String> {
    let path = config_dir.join("window-state.json");
    let data = match fs::read(&path) {
        Ok(data) => data,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(format!("read window placement: {error}")),
    };
    let placement: WindowPlacement = serde_json::from_slice(&data)
        .map_err(|error| format!("decode window placement: {error}"))?;
    Ok(validate_window_placement(placement))
}

fn validate_window_placement(placement: WindowPlacement) -> Option<WindowPlacement> {
    if !(800..=10_000).contains(&placement.width) || !(560..=10_000).contains(&placement.height) {
        return None;
    }
    Some(placement)
}

fn update_window_placement(handle: &AppHandle, event: &WindowEvent) {
    if !matches!(event, WindowEvent::Resized(_) | WindowEvent::Moved(_)) {
        return;
    }
    let Some(window) = handle.get_webview_window("main") else {
        return;
    };
    let Ok(maximized) = window.is_maximized() else {
        return;
    };
    let state = handle.state::<SidecarState>();
    let Ok(mut placement) = state.window_placement.lock() else {
        return;
    };
    let current = placement.get_or_insert_with(WindowPlacement::default);
    current.maximized = maximized;
    if !maximized {
        if let Ok(size) = window.inner_size() {
            current.width = size.width.max(800);
            current.height = size.height.max(560);
        }
    }
}

fn save_window_placement(handle: &AppHandle) -> Result<(), String> {
    if let Some(window) = handle.get_webview_window("main") {
        if let (Ok(maximized), Ok(size), Ok(mut placement)) = (
            window.is_maximized(),
            window.inner_size(),
            handle.state::<SidecarState>().window_placement.lock(),
        ) {
            let current = placement.get_or_insert_with(WindowPlacement::default);
            current.maximized = maximized;
            if !maximized {
                current.width = size.width.max(800);
                current.height = size.height.max(560);
            }
        }
    }
    let state = handle.state::<SidecarState>();
    let config_dir = state
        .desktop_paths
        .lock()
        .map_err(|_| "desktop paths are unavailable".to_string())?
        .as_ref()
        .map(|paths| paths.config_dir.clone())
        .ok_or_else(|| "desktop paths are unavailable".to_string())?;
    let placement = state
        .window_placement
        .lock()
        .map_err(|_| "window placement is unavailable".to_string())?
        .unwrap_or_default();
    fs::create_dir_all(&config_dir).map_err(|error| format!("create config directory: {error}"))?;
    let destination = config_dir.join("window-state.json");
    let temporary = config_dir.join(".window-state.json.tmp");
    let data = serde_json::to_vec(&placement)
        .map_err(|error| format!("encode window placement: {error}"))?;
    let mut file = fs::File::create(&temporary)
        .map_err(|error| format!("create window placement: {error}"))?;
    file.write_all(&data)
        .map_err(|error| format!("write window placement: {error}"))?;
    file.sync_all()
        .map_err(|error| format!("sync window placement: {error}"))?;
    drop(file);
    if let Err(error) = fs::rename(&temporary, &destination) {
        if destination.exists() {
            fs::remove_file(&destination)
                .map_err(|remove_error| format!("replace window placement: {remove_error}"))?;
            fs::rename(&temporary, &destination)
                .map_err(|rename_error| format!("replace window placement: {rename_error}"))?;
        } else {
            return Err(format!("replace window placement: {error}"));
        }
    }
    Ok(())
}

fn sidecar_arguments(paths: &DesktopPaths, app_version: &str) -> Vec<OsString> {
    let mut arguments = Vec::with_capacity(18);
    push_path_argument(&mut arguments, "--data-dir", &paths.data_dir);
    push_path_argument(&mut arguments, "--config-dir", &paths.config_dir);
    push_path_argument(&mut arguments, "--cache-dir", &paths.cache_dir);
    push_path_argument(&mut arguments, "--log-dir", &paths.log_dir);
    push_path_argument(&mut arguments, "--temp-dir", &paths.temp_dir);
    push_path_argument(&mut arguments, "--resource-dir", &paths.resource_dir);
    if let Some(runtime_dir) = paths.python_runtime_dir.as_ref() {
        push_path_argument(&mut arguments, "--python-runtime-dir", runtime_dir);
        push_path_argument(
            &mut arguments,
            "--python-executable",
            &runtime_dir.join("python.exe"),
        );
    }
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
    if matches!(
        *phase,
        StartupPhase::MaintenanceStopping
            | StartupPhase::Maintenance
            | StartupPhase::ShuttingDown
            | StartupPhase::Failed
    ) || state.maintenance_active.load(Ordering::SeqCst)
    {
        return;
    }
    *phase = StartupPhase::ShuttingDown;
    drop(phase);
    plugin_integration::core_unavailable(handle);

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
    plugin_integration::core_unavailable(handle);
    if let Ok(mut failure) = state.startup_failure.lock() {
        let classified = classify_startup_failure(message);
        if classified.code != "core_startup" || failure.is_none() {
            failure.replace(classified);
        }
    }
    if let Ok(mut child) = state.child.lock() {
        if let Some(child) = child.take() {
            let _ = child.kill();
        }
    }
    #[cfg(debug_assertions)]
    if smoke_enabled() {
        handle.exit(1);
        return;
    }
    if let Err(error) = show_startup_error_window(handle) {
        eprintln!("failed to show desktop startup error: {error}");
        handle.exit(1);
    }
}

fn classify_startup_failure(message: &str) -> StartupFailure {
    let normalized = message.to_ascii_lowercase();
    if normalized.contains("python") {
        return StartupFailure {
            code: "python_runtime",
            title: "The bundled Python runtime is unavailable",
            message: "The application package is incomplete or damaged. Extract or reinstall the complete Win64 package, then retry.",
        };
    }
    if normalized.contains("credential") || normalized.contains("keyring") {
        return StartupFailure {
            code: "credential_store",
            title: "Credential storage is unavailable",
            message: "Unlock the operating system credential store, then retry. Existing configuration was left unchanged.",
        };
    }
    if normalized.contains("protocol") || normalized.contains("version") {
        return StartupFailure {
            code: "version_mismatch",
            title: "Desktop components are incompatible",
            message: "The desktop shell and local core do not use compatible versions. Reinstall the same application version, then retry.",
        };
    }
    if normalized.contains("permission denied")
        || normalized.contains("access is denied")
        || normalized.contains("not writable")
        || normalized.contains("read-only")
        || normalized.contains("readonly")
        || normalized.contains("disk space")
        || normalized.contains("no space left")
        || normalized.contains("directory")
    {
        return StartupFailure {
            code: "local_storage",
            title: "Local storage is unavailable",
            message: "Check access to the application data directory, then retry. Existing data was not deleted.",
        };
    }
    if normalized.contains("database")
        || normalized.contains("sqlite")
        || normalized.contains("migration")
    {
        return StartupFailure {
            code: "local_data",
            title: "Local data could not be opened",
            message: "Review the application logs, restore or repair the affected data if needed, then retry. No automatic deletion was performed.",
        };
    }
    if normalized.contains("config") || normalized.contains("resource") {
        return StartupFailure {
            code: "local_configuration",
            title: "Local configuration could not be loaded",
            message: "Review the application logs for the affected file, correct or restore it, then retry. Your data was not deleted.",
        };
    }
    if normalized.contains("timed out") || normalized.contains("readiness") {
        return StartupFailure {
            code: "startup_timeout",
            title: "The local core did not become ready",
            message: "Another process or security tool may be delaying startup. Review the logs, then retry.",
        };
    }
    StartupFailure {
        code: "core_startup",
        title: "CyberStrikeAI could not start",
        message: "The local core stopped unexpectedly. Review the logs for details, then retry or exit safely.",
    }
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
        apply_handshake, classify_startup_failure, is_allowed_url, parse_handshake,
        sidecar_arguments, validate_window_placement, DesktopPaths, Handshake, StartupPhase,
        WindowPlacement,
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
            python_runtime_dir: Some(root.join("runtime").join("python")),
        };
        let arguments = sidecar_arguments(&paths, "0.1.0");
        let rendered = arguments
            .iter()
            .map(|value| value.to_string_lossy())
            .collect::<Vec<_>>()
            .join(" ");
        assert!(rendered.contains("--data-dir"));
        assert!(rendered.contains("--resource-dir"));
        assert!(rendered.contains("--python-runtime-dir"));
        assert!(rendered.contains("--python-executable"));
        assert!(rendered.contains("--app-version 0.1.0"));
        assert!(!rendered.to_lowercase().contains("password"));
    }

    #[test]
    fn startup_failures_are_mapped_to_safe_recovery_categories() {
        assert_eq!(
            classify_startup_failure("store desktop credential fofa.api_key").code,
            "credential_store"
        );
        assert_eq!(
            classify_startup_failure("unsupported desktop protocol version").code,
            "version_mismatch"
        );
        assert_eq!(
            classify_startup_failure("desktop Python dependency lock checksum mismatch").code,
            "python_runtime"
        );
        assert_eq!(
            classify_startup_failure("parse config file").code,
            "local_configuration"
        );
        assert_eq!(
            classify_startup_failure("prepare desktop data directory: access is denied").code,
            "local_storage"
        );
        assert_eq!(
            classify_startup_failure("insufficient disk space for desktop data operation").code,
            "local_storage"
        );
        assert_eq!(
            classify_startup_failure("database migration failed").code,
            "local_data"
        );
        let generic = classify_startup_failure("migration-secret");
        assert_eq!(generic.code, "local_data");
        assert!(!generic.message.contains("migration-secret"));
    }

    #[test]
    fn window_placement_rejects_unsafe_dimensions() {
        assert!(validate_window_placement(WindowPlacement {
            width: 1120,
            height: 760,
            maximized: true,
        })
        .is_some());
        assert!(validate_window_placement(WindowPlacement {
            width: 10,
            height: 10,
            maximized: false,
        })
        .is_none());
    }
}
