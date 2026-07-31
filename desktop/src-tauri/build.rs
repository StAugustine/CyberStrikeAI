fn main() {
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
        ]),
    ))
    .expect("failed to build CyberStrikeAI desktop metadata")
}
