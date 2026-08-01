use serde::{Deserialize, Serialize};
use std::{
    fs,
    io::{ErrorKind, Write},
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Mutex,
    },
    thread,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tauri::{AppHandle, Manager, WebviewWindow};

use super::{DesktopPaths, SidecarState, StartupPhase};

const SETTING_SCHEMA_VERSION: u32 = 1;
const DISCOVERY_SCHEMA_VERSION: u32 = 1;
const SETTING_FILE_NAME: &str = "plugin-integration.json";
const DISCOVERY_FILE_NAME: &str = "plugin-discovery.json";
const NATIVE_HOST_MANIFEST_FILE_NAME: &str = "plugin-native-host.json";
const NATIVE_HOST_NAME: &str = "com.cyberstrikeai.desktop";
const NATIVE_HOST_BINARY_BASENAME: &str = env!("CYBERSTRIKE_DESKTOP_NATIVE_HOST_BINARY_BASENAME");
const BROWSER_EXTENSION_ID: &str = "okialefpaaimfgjelpednbehgebgkdgo";
const DISCOVERY_REFRESH_INTERVAL: Duration = Duration::from_secs(30);
const DISCOVERY_LIFETIME: Duration = Duration::from_secs(90);

#[derive(Default)]
pub(crate) struct PluginIntegrationState {
    enabled: AtomicBool,
    browser_registered: AtomicBool,
    discovery_active: AtomicBool,
    refresh_generation: AtomicU64,
    instance_id: Mutex<Option<String>>,
}

#[derive(Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PluginIntegrationSetting {
    schema_version: u32,
    enabled: bool,
}

#[derive(Clone, Debug, Serialize)]
struct PluginDiscovery {
    schema_version: u32,
    instance_id: String,
    base_url: String,
    app_version: String,
    issued_at_unix: u64,
    expires_at_unix: u64,
}

#[derive(Debug, Serialize)]
struct NativeHostManifest {
    name: &'static str,
    description: &'static str,
    path: String,
    #[serde(rename = "type")]
    kind: &'static str,
    allowed_origins: Vec<String>,
}

#[derive(Debug, Serialize)]
pub(crate) struct PluginIntegrationStatus {
    enabled: bool,
    browser_registered: bool,
    discovery_active: bool,
    extension_id: &'static str,
}

pub(crate) fn initialize(handle: &AppHandle, paths: &DesktopPaths) {
    // A previous process may have exited before cleanup. Never carry its
    // still-unexpired endpoint across a desktop restart.
    remove_discovery(&paths.config_dir);
    let enabled = load_setting(&paths.config_dir).unwrap_or(false);
    let state = handle.state::<PluginIntegrationState>();
    state.enabled.store(enabled, Ordering::SeqCst);
    if !enabled {
        unregister_native_host(handle, &paths.config_dir);
        return;
    }
    match register_native_host(handle, &paths.config_dir) {
        Ok(()) => state.browser_registered.store(true, Ordering::SeqCst),
        Err(error) => {
            state.browser_registered.store(false, Ordering::SeqCst);
            eprintln!("desktop plugin native host registration failed: {error}");
        }
    }
}

pub(crate) fn core_ready(handle: &AppHandle, base_url: &str, app_version: &str) {
    let state = handle.state::<PluginIntegrationState>();
    if !state.enabled.load(Ordering::SeqCst) {
        return;
    }
    let Some(config_dir) = desktop_config_dir(handle) else {
        return;
    };
    start_discovery_refresh(
        handle,
        config_dir,
        base_url.to_string(),
        app_version.to_string(),
    );
}

pub(crate) fn core_unavailable(handle: &AppHandle) {
    let state = handle.state::<PluginIntegrationState>();
    state.refresh_generation.fetch_add(1, Ordering::SeqCst);
    state.discovery_active.store(false, Ordering::SeqCst);
    if let Ok(mut instance_id) = state.instance_id.lock() {
        instance_id.take();
    }
    if let Some(config_dir) = desktop_config_dir(handle) {
        remove_discovery(&config_dir);
    }
}

#[tauri::command]
pub(crate) fn get_plugin_integration_status(
    window: WebviewWindow,
) -> Result<PluginIntegrationStatus, String> {
    require_main_window(&window)?;
    let state = window.state::<PluginIntegrationState>();
    Ok(PluginIntegrationStatus {
        enabled: state.enabled.load(Ordering::SeqCst),
        browser_registered: state.browser_registered.load(Ordering::SeqCst),
        discovery_active: state.discovery_active.load(Ordering::SeqCst),
        extension_id: BROWSER_EXTENSION_ID,
    })
}

#[tauri::command]
pub(crate) fn set_plugin_integration_enabled(
    window: WebviewWindow,
    enabled: bool,
) -> Result<PluginIntegrationStatus, String> {
    require_main_window(&window)?;
    let handle = window.app_handle();
    let phase = handle
        .state::<SidecarState>()
        .phase
        .lock()
        .map_err(|_| "desktop plugin state is unavailable".to_string())?
        .to_owned();
    if phase != StartupPhase::Ready {
        return Err("desktop plugin integration requires a ready local instance".to_string());
    }
    let paths = handle
        .state::<SidecarState>()
        .desktop_paths
        .lock()
        .map_err(|_| "desktop plugin paths are unavailable".to_string())?
        .clone()
        .ok_or_else(|| "desktop plugin paths are unavailable".to_string())?;
    let state = handle.state::<PluginIntegrationState>();
    if enabled {
        let base_url = handle
            .state::<SidecarState>()
            .allowed_origin
            .lock()
            .map_err(|_| "desktop plugin endpoint is unavailable".to_string())?
            .clone()
            .ok_or_else(|| "desktop plugin endpoint is unavailable".to_string())?;
        register_native_host(handle, &paths.config_dir)?;
        if let Err(error) = persist_setting(&paths.config_dir, true) {
            unregister_native_host(handle, &paths.config_dir);
            return Err(error);
        }
        state.enabled.store(true, Ordering::SeqCst);
        state.browser_registered.store(true, Ordering::SeqCst);
        start_discovery_refresh(
            handle,
            paths.config_dir,
            base_url,
            handle.package_info().version.to_string(),
        );
    } else {
        persist_setting(&paths.config_dir, false)?;
        state.enabled.store(false, Ordering::SeqCst);
        core_unavailable(handle);
        unregister_native_host(handle, &paths.config_dir);
        state.browser_registered.store(false, Ordering::SeqCst);
    }
    get_plugin_integration_status(window)
}

fn require_main_window(window: &WebviewWindow) -> Result<(), String> {
    if window.label() != "main" {
        return Err("desktop plugin integration is not available to this window".to_string());
    }
    Ok(())
}

fn desktop_config_dir(handle: &AppHandle) -> Option<PathBuf> {
    handle
        .state::<SidecarState>()
        .desktop_paths
        .lock()
        .ok()?
        .as_ref()
        .map(|paths| paths.config_dir.clone())
}

fn start_discovery_refresh(
    handle: &AppHandle,
    config_dir: PathBuf,
    base_url: String,
    app_version: String,
) {
    let state = handle.state::<PluginIntegrationState>();
    let generation = state.refresh_generation.fetch_add(1, Ordering::SeqCst) + 1;
    let instance_id = new_instance_id(generation);
    if let Ok(mut stored) = state.instance_id.lock() {
        stored.replace(instance_id.clone());
    }
    let task_handle = handle.clone();
    thread::spawn(move || loop {
        let state = task_handle.state::<PluginIntegrationState>();
        if state.refresh_generation.load(Ordering::SeqCst) != generation
            || !state.enabled.load(Ordering::SeqCst)
        {
            return;
        }
        let now = unix_time_seconds();
        let discovery = PluginDiscovery {
            schema_version: DISCOVERY_SCHEMA_VERSION,
            instance_id: instance_id.clone(),
            base_url: base_url.clone(),
            app_version: app_version.clone(),
            issued_at_unix: now,
            expires_at_unix: now + DISCOVERY_LIFETIME.as_secs(),
        };
        match write_private_json_atomic(
            &config_dir,
            DISCOVERY_FILE_NAME,
            ".plugin-discovery.json.tmp",
            &discovery,
        ) {
            Ok(()) => state.discovery_active.store(true, Ordering::SeqCst),
            Err(error) => {
                state.discovery_active.store(false, Ordering::SeqCst);
                remove_discovery(&config_dir);
                eprintln!("desktop plugin discovery refresh failed: {error}");
            }
        }
        thread::sleep(DISCOVERY_REFRESH_INTERVAL);
    });
}

fn load_setting(config_dir: &Path) -> Result<bool, String> {
    let data = match fs::read(config_dir.join(SETTING_FILE_NAME)) {
        Ok(data) => data,
        Err(error) if error.kind() == ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err("desktop plugin setting is unavailable".to_string()),
    };
    let setting: PluginIntegrationSetting = serde_json::from_slice(&data)
        .map_err(|_| "desktop plugin setting is invalid".to_string())?;
    if setting.schema_version != SETTING_SCHEMA_VERSION {
        return Err("desktop plugin setting version is unsupported".to_string());
    }
    Ok(setting.enabled)
}

fn persist_setting(config_dir: &Path, enabled: bool) -> Result<(), String> {
    write_private_json_atomic(
        config_dir,
        SETTING_FILE_NAME,
        ".plugin-integration.json.tmp",
        &PluginIntegrationSetting {
            schema_version: SETTING_SCHEMA_VERSION,
            enabled,
        },
    )
}

fn write_private_json_atomic<T: Serialize>(
    directory: &Path,
    file_name: &str,
    temporary_name: &str,
    value: &T,
) -> Result<(), String> {
    fs::create_dir_all(directory)
        .map_err(|_| "desktop plugin directory is unavailable".to_string())?;
    let destination = directory.join(file_name);
    let temporary = directory.join(temporary_name);
    let mut data = serde_json::to_vec_pretty(value)
        .map_err(|_| "desktop plugin metadata could not be encoded".to_string())?;
    data.push(b'\n');
    let mut file = fs::File::create(&temporary)
        .map_err(|_| "desktop plugin metadata could not be created".to_string())?;
    protect_private_file(&file)?;
    file.write_all(&data)
        .map_err(|_| "desktop plugin metadata could not be written".to_string())?;
    file.sync_all()
        .map_err(|_| "desktop plugin metadata could not be synchronized".to_string())?;
    drop(file);
    replace_file(&temporary, &destination)?;
    Ok(())
}

fn replace_file(temporary: &Path, destination: &Path) -> Result<(), String> {
    if let Err(error) = fs::rename(temporary, destination) {
        if destination.exists() {
            fs::remove_file(destination)
                .map_err(|_| "desktop plugin metadata could not be replaced".to_string())?;
            fs::rename(temporary, destination)
                .map_err(|_| "desktop plugin metadata could not be replaced".to_string())?;
        } else {
            return Err(format!(
                "desktop plugin metadata could not be published: {error}"
            ));
        }
    }
    Ok(())
}

#[cfg(unix)]
fn protect_private_file(file: &fs::File) -> Result<(), String> {
    use std::os::unix::fs::PermissionsExt;
    file.set_permissions(fs::Permissions::from_mode(0o600))
        .map_err(|_| "desktop plugin metadata permissions could not be protected".to_string())
}

#[cfg(windows)]
fn protect_private_file(_file: &fs::File) -> Result<(), String> {
    Ok(())
}

fn remove_discovery(config_dir: &Path) {
    match fs::remove_file(config_dir.join(DISCOVERY_FILE_NAME)) {
        Ok(()) => {}
        Err(error) if error.kind() == ErrorKind::NotFound => {}
        Err(error) => eprintln!("desktop plugin discovery cleanup failed: {error}"),
    }
}

fn new_instance_id(generation: u64) -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    format!("desktop-{:x}-{nanos:x}-{generation:x}", std::process::id())
}

fn unix_time_seconds() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

fn native_host_manifest(executable: &Path) -> Result<NativeHostManifest, String> {
    if !executable.is_absolute() || !executable.is_file() {
        return Err("desktop plugin native host is unavailable".to_string());
    }
    Ok(NativeHostManifest {
        name: NATIVE_HOST_NAME,
        description: "CyberStrikeAI Desktop local instance discovery",
        path: executable.to_string_lossy().into_owned(),
        kind: "stdio",
        allowed_origins: vec![format!("chrome-extension://{BROWSER_EXTENSION_ID}/")],
    })
}

fn native_host_executable() -> Result<PathBuf, String> {
    let executable = std::env::current_exe()
        .map_err(|_| "desktop application path is unavailable".to_string())?;
    let directory = executable
        .parent()
        .ok_or_else(|| "desktop application path is unavailable".to_string())?;
    let name = if cfg!(windows) {
        format!("{NATIVE_HOST_BINARY_BASENAME}.exe")
    } else {
        NATIVE_HOST_BINARY_BASENAME.to_string()
    };
    Ok(directory.join(name))
}

fn register_native_host(handle: &AppHandle, config_dir: &Path) -> Result<(), String> {
    let manifest = native_host_manifest(&native_host_executable()?)?;
    if let Err(error) = write_private_json_atomic(
        config_dir,
        NATIVE_HOST_MANIFEST_FILE_NAME,
        ".plugin-native-host.json.tmp",
        &manifest,
    ) {
        unregister_native_host(handle, config_dir);
        return Err(error);
    }
    let manifest_path = config_dir.join(NATIVE_HOST_MANIFEST_FILE_NAME);
    if let Err(error) = register_browser_manifests(handle, &manifest_path, &manifest) {
        unregister_native_host(handle, config_dir);
        return Err(error);
    }
    Ok(())
}

fn unregister_native_host(handle: &AppHandle, config_dir: &Path) {
    unregister_browser_manifests(handle);
    let _ = fs::remove_file(config_dir.join(NATIVE_HOST_MANIFEST_FILE_NAME));
}

#[cfg(target_os = "macos")]
fn browser_manifest_paths(handle: &AppHandle) -> Result<Vec<PathBuf>, String> {
    let home = handle
        .path()
        .home_dir()
        .map_err(|_| "desktop browser profile root is unavailable".to_string())?;
    Ok(vec![
        home.join("Library/Application Support/Google/Chrome/NativeMessagingHosts")
            .join(format!("{NATIVE_HOST_NAME}.json")),
        home.join("Library/Application Support/Microsoft Edge/NativeMessagingHosts")
            .join(format!("{NATIVE_HOST_NAME}.json")),
    ])
}

#[cfg(target_os = "macos")]
fn register_browser_manifests(
    handle: &AppHandle,
    _manifest_path: &Path,
    manifest: &NativeHostManifest,
) -> Result<(), String> {
    for destination in browser_manifest_paths(handle)? {
        let directory = destination
            .parent()
            .ok_or_else(|| "desktop browser manifest path is invalid".to_string())?;
        let file_name = destination
            .file_name()
            .and_then(|name| name.to_str())
            .ok_or_else(|| "desktop browser manifest path is invalid".to_string())?;
        write_private_json_atomic(
            directory,
            file_name,
            ".cyberstrike-native-host.tmp",
            manifest,
        )?;
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn unregister_browser_manifests(handle: &AppHandle) {
    if let Ok(paths) = browser_manifest_paths(handle) {
        for path in paths {
            let _ = fs::remove_file(path);
        }
    }
}

#[cfg(windows)]
fn windows_registry_keys() -> [&'static str; 2] {
    [
        r"HKCU\Software\Google\Chrome\NativeMessagingHosts\com.cyberstrikeai.desktop",
        r"HKCU\Software\Microsoft\Edge\NativeMessagingHosts\com.cyberstrikeai.desktop",
    ]
}

#[cfg(windows)]
fn register_browser_manifests(
    _handle: &AppHandle,
    manifest_path: &Path,
    _manifest: &NativeHostManifest,
) -> Result<(), String> {
    for key in windows_registry_keys() {
        let status = std::process::Command::new("reg.exe")
            .args(["ADD", key, "/ve", "/t", "REG_SZ", "/d"])
            .arg(manifest_path)
            .arg("/f")
            .status()
            .map_err(|_| "desktop browser registration could not be started".to_string())?;
        if !status.success() {
            return Err("desktop browser registration failed".to_string());
        }
    }
    Ok(())
}

#[cfg(windows)]
fn unregister_browser_manifests(_handle: &AppHandle) {
    for key in windows_registry_keys() {
        let _ = std::process::Command::new("reg.exe")
            .args(["DELETE", key, "/f"])
            .status();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn discovery_and_native_manifest_never_contain_credentials() {
        let discovery = PluginDiscovery {
            schema_version: DISCOVERY_SCHEMA_VERSION,
            instance_id: "desktop-instance-123456".to_string(),
            base_url: "http://127.0.0.1:43123".to_string(),
            app_version: "0.1.0".to_string(),
            issued_at_unix: 1_800_000_000,
            expires_at_unix: 1_800_000_090,
        };
        let encoded = serde_json::to_string(&discovery).unwrap();
        for forbidden in ["password", "token", "credential", "session"] {
            assert!(!encoded.to_ascii_lowercase().contains(forbidden));
        }

        let root = test_directory("native-host");
        let executable = root.join(if cfg!(windows) { "host.exe" } else { "host" });
        fs::create_dir_all(&root).unwrap();
        fs::write(&executable, b"fixture").unwrap();
        let manifest = native_host_manifest(&executable).unwrap();
        assert_eq!(
            manifest.allowed_origins,
            vec![format!("chrome-extension://{BROWSER_EXTENSION_ID}/")]
        );
        let _ = fs::remove_file(executable);
        let _ = fs::remove_dir(root);
    }

    #[test]
    fn native_host_path_uses_the_compiled_binary_name() {
        let executable = native_host_executable().unwrap();
        let expected = if cfg!(windows) {
            format!("{NATIVE_HOST_BINARY_BASENAME}.exe")
        } else {
            NATIVE_HOST_BINARY_BASENAME.to_string()
        };
        assert_eq!(
            executable.file_name().and_then(|name| name.to_str()),
            Some(expected.as_str())
        );
    }

    #[test]
    fn private_metadata_round_trips_atomically() {
        let root = test_directory("metadata");
        persist_setting(&root, true).unwrap();
        assert!(load_setting(&root).unwrap());
        persist_setting(&root, false).unwrap();
        assert!(!load_setting(&root).unwrap());
        let _ = fs::remove_file(root.join(SETTING_FILE_NAME));
        let _ = fs::remove_dir(root);
    }

    #[test]
    fn plugin_setting_rejects_unknown_or_secret_fields() {
        let root = test_directory("setting-fields");
        fs::create_dir_all(&root).unwrap();
        fs::write(
            root.join(SETTING_FILE_NAME),
            br#"{"schema_version":1,"enabled":true,"token":"forbidden"}"#,
        )
        .unwrap();
        assert!(load_setting(&root).is_err());
        let _ = fs::remove_file(root.join(SETTING_FILE_NAME));
        let _ = fs::remove_dir(root);
    }

    fn test_directory(label: &str) -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../.tmp")
            .join(format!(
                "cyberstrike-plugin-{label}-{}-{}",
                std::process::id(),
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_nanos()
            ))
    }
}
