fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "get_credential_migration_paths",
            "confirm_credential_migration",
            "cancel_credential_migration",
            "submit_bootstrap_password",
        ]),
    ))
    .expect("failed to build CyberStrikeAI desktop metadata")
}
