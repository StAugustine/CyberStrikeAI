fn main() {
    tauri_build::try_build(
        tauri_build::Attributes::new()
            .app_manifest(tauri_build::AppManifest::new().commands(&["submit_bootstrap_password"])),
    )
    .expect("failed to build CyberStrikeAI desktop metadata")
}
