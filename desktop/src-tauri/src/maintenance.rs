use super::{
    fail_sidecar, is_app_asset_url, sidecar_arguments, start_sidecar, DesktopPaths, SidecarState,
    StartupPhase, FORCE_EXIT_TIMEOUT, SIDECAR_NAME,
};
use serde_json::Value;
use std::{
    ffi::OsString,
    path::Path,
    sync::{
        atomic::Ordering,
        mpsc::{self, RecvTimeoutError},
    },
};
use tauri::{
    webview::{DownloadEvent, NewWindowResponse},
    AppHandle, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder,
};
use tauri_plugin_dialog::DialogExt;
use tauri_plugin_shell::ShellExt;

enum MaintenanceOperation<'a> {
    ListBackups,
    PrepareImport(&'a Path),
    CommitImport,
    CancelImport,
    RestoreBackup(&'a str),
    DeleteBackup(&'a str),
}

struct MaintenanceActivity<'a>(&'a std::sync::atomic::AtomicBool);

impl Drop for MaintenanceActivity<'_> {
    fn drop(&mut self) {
        self.0.store(false, Ordering::SeqCst);
    }
}

impl MaintenanceOperation<'_> {
    fn name(&self) -> &'static str {
        match self {
            Self::ListBackups => "list-backups",
            Self::PrepareImport(_) => "prepare-import",
            Self::CommitImport => "commit-import",
            Self::CancelImport => "cancel-import",
            Self::RestoreBackup(_) => "restore-backup",
            Self::DeleteBackup(_) => "delete-backup",
        }
    }
}

#[tauri::command]
pub(crate) fn open_data_maintenance(window: WebviewWindow) -> Result<(), String> {
    if window.label() != "main" {
        return Err("data maintenance is not available to this window".to_string());
    }
    let handle = window.app_handle();
    require_phase(handle, StartupPhase::Ready)?;

    if let Some(maintenance) = handle.get_webview_window("data-maintenance") {
        set_maintenance_hold(handle, true)?;
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

    WebviewWindowBuilder::new(
        handle,
        "data-maintenance",
        WebviewUrl::App("data-maintenance.html".into()),
    )
    .title("CyberStrikeAI data maintenance")
    .inner_size(900.0, 720.0)
    .min_inner_size(720.0, 600.0)
    .resizable(true)
    .maximizable(true)
    .minimizable(true)
    .closable(false)
    .center()
    .on_navigation(is_app_asset_url)
    .on_new_window(|_url, _features| NewWindowResponse::Deny)
    .on_download(|_webview, event| !matches!(event, DownloadEvent::Requested { .. }))
    .build()
    .map_err(|error| format!("create data maintenance window: {error}"))?;
    set_maintenance_hold(handle, true)?;
    if let Err(error) = window.hide() {
        set_maintenance_hold(handle, false)?;
        if let Some(maintenance) = handle.get_webview_window("data-maintenance") {
            let _ = maintenance.destroy();
        }
        return Err(format!("hide main window: {error}"));
    }
    Ok(())
}

#[tauri::command]
pub(crate) async fn get_data_maintenance_state(window: WebviewWindow) -> Result<Value, String> {
    require_maintenance_window(&window)?;
    require_phase(window.app_handle(), StartupPhase::Ready)?;
    run_maintenance_sidecar(window.app_handle(), MaintenanceOperation::ListBackups).await
}

#[tauri::command]
pub(crate) async fn choose_and_prepare_legacy_import(
    window: WebviewWindow,
) -> Result<Option<Value>, String> {
    require_maintenance_window(&window)?;
    require_phase(window.app_handle(), StartupPhase::Ready)?;
    let selected = window
        .dialog()
        .file()
        .set_parent(&window)
        .set_title("Select a CyberStrikeAI v1.7.x data directory")
        .blocking_pick_folder();
    let Some(selected) = selected else {
        return Ok(None);
    };
    let source = selected
        .into_path()
        .map_err(|_| "the selected folder is unavailable".to_string())?;
    if !source.is_absolute() {
        return Err("the selected folder is invalid".to_string());
    }
    let response = run_maintenance_sidecar(
        window.app_handle(),
        MaintenanceOperation::PrepareImport(&source),
    )
    .await?;
    Ok(Some(response))
}

#[tauri::command]
pub(crate) async fn confirm_legacy_import(window: WebviewWindow) -> Result<Value, String> {
    require_maintenance_window(&window)?;
    run_offline_maintenance(window.app_handle(), MaintenanceOperation::CommitImport).await
}

#[tauri::command]
pub(crate) async fn cancel_legacy_import(window: WebviewWindow) -> Result<Value, String> {
    require_maintenance_window(&window)?;
    require_phase(window.app_handle(), StartupPhase::Ready)?;
    run_maintenance_sidecar(window.app_handle(), MaintenanceOperation::CancelImport).await
}

#[tauri::command]
pub(crate) async fn restore_desktop_backup(
    window: WebviewWindow,
    backup_id: String,
) -> Result<Value, String> {
    require_maintenance_window(&window)?;
    validate_backup_id(&backup_id)?;
    run_offline_maintenance(
        window.app_handle(),
        MaintenanceOperation::RestoreBackup(&backup_id),
    )
    .await
}

#[tauri::command]
pub(crate) async fn delete_desktop_backup(
    window: WebviewWindow,
    backup_id: String,
) -> Result<Value, String> {
    require_maintenance_window(&window)?;
    require_phase(window.app_handle(), StartupPhase::Ready)?;
    validate_backup_id(&backup_id)?;
    run_maintenance_sidecar(
        window.app_handle(),
        MaintenanceOperation::DeleteBackup(&backup_id),
    )
    .await
}

#[tauri::command]
pub(crate) fn close_data_maintenance(window: WebviewWindow) -> Result<(), String> {
    require_maintenance_window(&window)?;
    require_phase(window.app_handle(), StartupPhase::Ready)?;
    if window
        .state::<SidecarState>()
        .maintenance_active
        .load(Ordering::SeqCst)
    {
        return Err("wait for the current data maintenance operation to finish".to_string());
    }
    let main = window
        .app_handle()
        .get_webview_window("main")
        .ok_or_else(|| "main window is unavailable".to_string())?;
    main.show()
        .map_err(|error| format!("show main window: {error}"))?;
    main.set_focus()
        .map_err(|error| format!("focus main window: {error}"))?;
    set_maintenance_hold(window.app_handle(), false)?;
    window
        .destroy()
        .map_err(|error| format!("close data maintenance window: {error}"))
}

pub(super) fn finish_core_stop(handle: &AppHandle, result: Result<(), String>) {
    let state = handle.state::<SidecarState>();
    if let Ok(mut phase) = state.phase.lock() {
        if *phase == StartupPhase::MaintenanceStopping {
            *phase = StartupPhase::Maintenance;
        }
    }
    if let Ok(mut waiter) = state.maintenance_waiter.lock() {
        if let Some(waiter) = waiter.take() {
            let _ = waiter.send(result);
        }
    };
}

pub(super) fn maintenance_holds_main(handle: &AppHandle) -> bool {
    handle
        .state::<SidecarState>()
        .maintenance_holds_main
        .lock()
        .map(|hold| *hold)
        .unwrap_or(false)
}

fn require_maintenance_window(window: &WebviewWindow) -> Result<(), String> {
    if window.label() != "data-maintenance" {
        return Err("data maintenance is not available to this window".to_string());
    }
    Ok(())
}

fn require_phase(handle: &AppHandle, expected: StartupPhase) -> Result<(), String> {
    let state = handle.state::<SidecarState>();
    let phase = state
        .phase
        .lock()
        .map_err(|_| "desktop state is unavailable".to_string())?;
    if *phase != expected {
        return Err("desktop data maintenance is not currently available".to_string());
    }
    Ok(())
}

fn set_maintenance_hold(handle: &AppHandle, hold: bool) -> Result<(), String> {
    *handle
        .state::<SidecarState>()
        .maintenance_holds_main
        .lock()
        .map_err(|_| "data maintenance window state is unavailable".to_string())? = hold;
    Ok(())
}

fn validate_backup_id(backup_id: &str) -> Result<(), String> {
    if backup_id.is_empty()
        || backup_id.len() > 200
        || backup_id.trim() != backup_id
        || backup_id == "."
        || backup_id == ".."
        || backup_id.contains(['/', '\\'])
        || !backup_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err("the selected backup is invalid".to_string());
    }
    Ok(())
}

async fn run_offline_maintenance(
    handle: &AppHandle,
    operation: MaintenanceOperation<'_>,
) -> Result<Value, String> {
    if let Err(error) = stop_core_for_maintenance(handle).await {
        let stopped = handle
            .state::<SidecarState>()
            .phase
            .lock()
            .map(|phase| *phase == StartupPhase::Maintenance)
            .unwrap_or(false);
        if stopped {
            restart_core_after_maintenance(handle)?;
        }
        return Err(error);
    }

    let result = run_maintenance_sidecar(handle, operation).await;
    if result.is_ok() {
        set_maintenance_hold(handle, false)?;
    }
    restart_core_after_maintenance(handle)?;
    result
}

async fn stop_core_for_maintenance(handle: &AppHandle) -> Result<(), String> {
    let state = handle.state::<SidecarState>();
    let (sender, receiver) = mpsc::channel();
    {
        let mut phase = state
            .phase
            .lock()
            .map_err(|_| "desktop state is unavailable".to_string())?;
        if *phase != StartupPhase::Ready {
            return Err("desktop data maintenance is not currently available".to_string());
        }
        let mut waiter = state
            .maintenance_waiter
            .lock()
            .map_err(|_| "desktop maintenance state is unavailable".to_string())?;
        if waiter.is_some() {
            return Err("another desktop maintenance operation is already running".to_string());
        }
        let write_result = state
            .child
            .lock()
            .map_err(|_| "desktop core is unavailable".to_string())?
            .as_mut()
            .ok_or_else(|| "desktop core is unavailable".to_string())?
            .write(b"{\"type\":\"SHUTDOWN\",\"protocol_version\":1}\n");
        *phase = StartupPhase::MaintenanceStopping;
        waiter.replace(sender);
        if write_result.is_err() {
            waiter.take();
            *phase = StartupPhase::Maintenance;
            drop(waiter);
            drop(phase);
            state
                .generation
                .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            if let Ok(mut child) = state.child.lock() {
                if let Some(child) = child.take() {
                    let _ = child.kill();
                }
            }
            return Err("the local core could not be stopped safely".to_string());
        }
    }

    let waited =
        tauri::async_runtime::spawn_blocking(move || receiver.recv_timeout(FORCE_EXIT_TIMEOUT))
            .await
            .map_err(|_| "waiting for the local core failed".to_string())?;
    match waited {
        Ok(result) => result,
        Err(RecvTimeoutError::Timeout) => {
            state
                .generation
                .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            if let Ok(mut child) = state.child.lock() {
                if let Some(child) = child.take() {
                    let _ = child.kill();
                }
            }
            if let Ok(mut waiter) = state.maintenance_waiter.lock() {
                waiter.take();
            }
            if let Ok(mut phase) = state.phase.lock() {
                *phase = StartupPhase::Maintenance;
            }
            Err("the local core did not stop in time; no data was changed".to_string())
        }
        Err(RecvTimeoutError::Disconnected) => {
            Err("the local core stop confirmation was unavailable".to_string())
        }
    }
}

fn restart_core_after_maintenance(handle: &AppHandle) -> Result<(), String> {
    let state = handle.state::<SidecarState>();
    let paths = state
        .desktop_paths
        .lock()
        .map_err(|_| "desktop paths are unavailable".to_string())?
        .clone()
        .ok_or_else(|| "desktop paths are unavailable".to_string())?;
    {
        let mut phase = state
            .phase
            .lock()
            .map_err(|_| "desktop state is unavailable".to_string())?;
        if *phase != StartupPhase::Maintenance {
            return Err("desktop core is not ready to restart".to_string());
        }
        *phase = StartupPhase::Starting;
    }
    if let Err(error) = start_sidecar(handle, paths) {
        fail_sidecar(handle, &error.to_string());
        return Err("the local core could not be restarted".to_string());
    }
    Ok(())
}

async fn run_maintenance_sidecar(
    handle: &AppHandle,
    operation: MaintenanceOperation<'_>,
) -> Result<Value, String> {
    let state = handle.state::<SidecarState>();
    state
        .maintenance_active
        .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
        .map_err(|_| "another desktop maintenance operation is already running".to_string())?;
    let _activity = MaintenanceActivity(&state.maintenance_active);
    let paths = state
        .desktop_paths
        .lock()
        .map_err(|_| "desktop paths are unavailable".to_string())?
        .clone()
        .ok_or_else(|| "desktop paths are unavailable".to_string())?;
    let app_version = handle.package_info().version.to_string();
    let arguments = maintenance_arguments(&paths, &app_version, &operation);
    let output = handle
        .shell()
        .sidecar(SIDECAR_NAME)
        .map_err(|_| "desktop maintenance could not be started".to_string())?
        .args(arguments)
        .output()
        .await
        .map_err(|_| "desktop maintenance could not be completed".to_string())?;
    if !output.status.success() {
        let details = String::from_utf8_lossy(&output.stderr);
        eprintln!("desktop maintenance {} failed: {details}", operation.name());
        return Err(classify_maintenance_error(&details));
    }
    let mut response: Value = serde_json::from_slice(&output.stdout).map_err(|_| {
        eprintln!(
            "desktop maintenance {} returned invalid JSON",
            operation.name()
        );
        "desktop maintenance returned an invalid result".to_string()
    })?;
    sanitize_maintenance_response(&mut response, &operation);
    Ok(response)
}

fn sanitize_maintenance_response(response: &mut Value, operation: &MaintenanceOperation<'_>) {
    if let Some(report) = response
        .get_mut("import_report")
        .and_then(Value::as_object_mut)
    {
        report.remove("source_name");
    }
    if let Some(report) = response
        .get_mut("pending_import")
        .and_then(Value::as_object_mut)
        .and_then(|pending| pending.get_mut("report"))
        .and_then(Value::as_object_mut)
    {
        report.remove("source_name");
    }
    if matches!(operation, MaintenanceOperation::ListBackups) {
        if let Some(backups) = response.get_mut("backups").and_then(Value::as_array_mut) {
            for backup in backups {
                if let Some(backup) = backup.as_object_mut() {
                    backup.remove("error");
                }
            }
        }
    }
}

fn maintenance_arguments(
    paths: &DesktopPaths,
    app_version: &str,
    operation: &MaintenanceOperation<'_>,
) -> Vec<OsString> {
    let mut arguments = sidecar_arguments(paths, app_version);
    arguments.push(OsString::from("--maintenance"));
    arguments.push(OsString::from(operation.name()));
    match operation {
        MaintenanceOperation::PrepareImport(source) => {
            arguments.push(OsString::from("--source-dir"));
            arguments.push(source.as_os_str().to_os_string());
        }
        MaintenanceOperation::RestoreBackup(backup_id)
        | MaintenanceOperation::DeleteBackup(backup_id) => {
            arguments.push(OsString::from("--backup-id"));
            arguments.push(OsString::from(backup_id));
        }
        _ => {}
    }
    arguments
}

fn classify_maintenance_error(message: &str) -> String {
    let normalized = message.to_ascii_lowercase();
    if normalized.contains("unsupported") && normalized.contains("version") {
        return "The selected data is not from a supported CyberStrikeAI v1.7.x release."
            .to_string();
    }
    if normalized.contains("symbolic")
        || normalized.contains("symlink")
        || normalized.contains("special file")
        || normalized.contains("escapes")
        || normalized.contains("relative path")
    {
        return "The selected folder contains an unsafe path or unsupported file type.".to_string();
    }
    if normalized.contains("changed while") {
        return "The selected data changed during inspection. Stop the old instance and try again."
            .to_string();
    }
    if normalized.contains("sqlite") || normalized.contains("database") {
        return "A local database could not be verified. Existing desktop data was not replaced."
            .to_string();
    }
    if normalized.contains("retention") || normalized.contains("protected") {
        return "This recovery point is protected by the backup retention policy.".to_string();
    }
    if normalized.contains("pending") {
        return "Finish or cancel the pending data operation before continuing.".to_string();
    }
    if normalized.contains("permission")
        || normalized.contains("access is denied")
        || normalized.contains("not writable")
    {
        return "The selected data or desktop storage is not accessible.".to_string();
    }
    "Desktop data maintenance could not be completed. Existing data was left unchanged.".to_string()
}

#[cfg(test)]
mod tests {
    use super::{
        classify_maintenance_error, maintenance_arguments, sanitize_maintenance_response,
        validate_backup_id, MaintenanceOperation,
    };
    use crate::DesktopPaths;
    use std::path::PathBuf;

    fn paths() -> DesktopPaths {
        let root = PathBuf::from("/tmp/cyberstrike-maintenance-test");
        DesktopPaths {
            data_dir: root.join("data"),
            config_dir: root.join("config"),
            cache_dir: root.join("cache"),
            log_dir: root.join("logs"),
            temp_dir: root.join("temp"),
            resource_dir: root.join("resources"),
        }
    }

    #[test]
    fn maintenance_arguments_are_fixed_and_do_not_use_a_shell() {
        let source = PathBuf::from("/tmp/legacy data");
        let arguments = maintenance_arguments(
            &paths(),
            "0.1.0",
            &MaintenanceOperation::PrepareImport(&source),
        );
        let rendered = arguments
            .iter()
            .map(|value| value.to_string_lossy())
            .collect::<Vec<_>>();
        assert!(rendered
            .windows(2)
            .any(|pair| pair == ["--maintenance", "prepare-import"]));
        assert!(rendered
            .windows(2)
            .any(|pair| pair == ["--source-dir", "/tmp/legacy data"]));
        assert!(!rendered.iter().any(|value| value.contains("sh -c")));
    }

    #[test]
    fn backup_ids_cannot_select_paths() {
        assert!(validate_backup_id("upgrade-20260731T120000Z").is_ok());
        for invalid in [
            "",
            "..",
            "../backup",
            "folder/backup",
            "folder\\backup",
            " id",
        ] {
            assert!(validate_backup_id(invalid).is_err(), "accepted {invalid:?}");
        }
    }

    #[test]
    fn raw_maintenance_failures_are_mapped_to_safe_messages() {
        let source = "/Users/example/private/legacy";
        let classified = classify_maintenance_error(&format!(
            "open sqlite database at {source}: database is locked"
        ));
        assert!(classified.contains("database"));
        assert!(!classified.contains(source));
        assert!(classify_maintenance_error("symbolic link rejected").contains("unsafe path"));
        assert!(classify_maintenance_error("protected by retention").contains("retention"));
    }

    #[test]
    fn backup_catalog_does_not_expose_verification_details() {
        let mut response = serde_json::json!({
            "import_report": {"source_name": "private-source"},
            "pending_import": {"report": {"source_name": "private-source"}},
            "backups": [{
                "id": "broken",
                "valid": false,
                "error": "read /Users/example/private/manifest.json: permission denied"
            }]
        });
        sanitize_maintenance_response(&mut response, &MaintenanceOperation::ListBackups);
        assert!(response["backups"][0].get("error").is_none());
        assert!(response["pending_import"]["report"]
            .get("source_name")
            .is_none());
        assert!(response["import_report"].get("source_name").is_none());
    }
}
