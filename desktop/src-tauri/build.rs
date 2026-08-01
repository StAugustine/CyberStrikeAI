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
