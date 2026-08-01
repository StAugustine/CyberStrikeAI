use serde_json::Value;
use std::{env, fs, path::PathBuf};

fn main() {
    let (core_binary_basename, native_host_binary_basename) = desktop_binary_names();
    println!(
        "cargo:rustc-env=CYBERSTRIKE_DESKTOP_CORE_BINARY_BASENAME={core_binary_basename}"
    );
    println!(
        "cargo:rustc-env=CYBERSTRIKE_DESKTOP_NATIVE_HOST_BINARY_BASENAME={native_host_binary_basename}"
    );
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "get_credential_migration_paths",
            "confirm_credential_migration",
            "cancel_credential_migration",
            "submit_bootstrap_password",
            "get_startup_failure",
            "retry_startup",
            "exit_after_startup_failure",
            "open_desktop_directory",
            "open_data_maintenance",
            "get_data_maintenance_state",
            "choose_and_prepare_legacy_import",
            "confirm_legacy_import",
            "cancel_legacy_import",
            "restore_desktop_backup",
            "delete_desktop_backup",
            "close_data_maintenance",
            "get_plugin_integration_status",
            "set_plugin_integration_enabled",
        ]),
    ))
    .expect("failed to build CyberStrikeAI desktop metadata")
}

fn desktop_binary_names() -> (String, String) {
    let manifest_dir =
        PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("Cargo manifest directory"));
    let config_path = manifest_dir
        .parent()
        .expect("desktop source directory")
        .join("build-config.json");
    println!("cargo:rerun-if-changed={}", config_path.display());
    let data = fs::read(&config_path).expect("read desktop build configuration");
    let config: Value = serde_json::from_slice(&data).expect("decode desktop build configuration");
    let object = config
        .as_object()
        .expect("desktop build configuration must be an object");
    for key in object.keys() {
        assert!(
            matches!(
                key.as_str(),
                "schema_version" | "core_binary_basename" | "native_host_binary_basename"
            ),
            "unsupported desktop build configuration field: {key}"
        );
    }
    assert_eq!(
        object.get("schema_version").and_then(Value::as_u64),
        Some(1),
        "unsupported desktop build configuration version"
    );
    let configured_core = validate_binary_basename(
        object
            .get("core_binary_basename")
            .and_then(Value::as_str)
            .expect("core_binary_basename must be a string"),
        "core_binary_basename",
    );
    let configured_native_host = validate_binary_basename(
        object
            .get("native_host_binary_basename")
            .and_then(Value::as_str)
            .expect("native_host_binary_basename must be a string"),
        "native_host_binary_basename",
    );
    assert_ne!(
        configured_core.to_ascii_lowercase(),
        configured_native_host.to_ascii_lowercase(),
        "desktop core and native host binary names must be different"
    );
    if env::var("CARGO_CFG_TARGET_OS").ok().as_deref() == Some("windows") {
        (configured_core, configured_native_host)
    } else {
        (
            "cyberstrike-core".to_string(),
            "cyberstrike-native-host".to_string(),
        )
    }
}

fn validate_binary_basename(value: &str, field: &str) -> String {
    assert!(
        !value.is_empty() && value.len() <= 64,
        "{field} must contain between 1 and 64 characters"
    );
    assert!(
        value
            .bytes()
            .enumerate()
            .all(|(index, byte)| byte.is_ascii_alphanumeric()
                || (index > 0 && matches!(byte, b'.' | b'_' | b'-')))
            && !value.ends_with('.'),
        "{field} must be a safe executable basename"
    );
    assert!(
        !value.to_ascii_lowercase().ends_with(".exe"),
        "{field} must not include the .exe extension"
    );
    let reserved = value
        .split('.')
        .next()
        .unwrap_or_default()
        .to_ascii_uppercase();
    assert!(
        !matches!(reserved.as_str(), "CON" | "PRN" | "AUX" | "NUL")
            && !(reserved.len() == 4
                && (reserved.starts_with("COM") || reserved.starts_with("LPT"))
                && matches!(reserved.as_bytes()[3], b'1'..=b'9')),
        "{field} uses a reserved Windows filename"
    );
    assert_ne!(
        value.to_ascii_lowercase(),
        "cyberstrike-desktop",
        "{field} conflicts with the desktop application executable"
    );
    value.to_string()
}
