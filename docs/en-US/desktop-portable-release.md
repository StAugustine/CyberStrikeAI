# Desktop portable release

CyberStrikeAI Desktop 0.2.0 is delivered as unsigned development-candidate ZIP archives. There is no installer or automatic updater. The supported targets are Windows 10/11 x64 with WebView2 Evergreen Runtime, macOS 12+ arm64, and macOS 12+ x64.

## Start and upgrade

Keep the archive layout intact. On Windows, extract the ZIP and run `CyberStrikeAI Desktop.exe`. On macOS, extract it and open `CyberStrikeAI Desktop.app`. The main executable, Go core, native-messaging host, and `defaults/` resources are one versioned unit.

To replace an older portable build:

1. Exit CyberStrikeAI completely.
2. Keep the operating-system application data and configuration directories unchanged.
3. Delete or archive only the old extracted program directory.
4. Extract the new ZIP into a new program directory and start it.

The first R2 start reuses the R1 data root, creates a verified pre-upgrade recovery point, installs versioned defaults without overwriting user-modified resources, and keeps configuration and system-credential-store references. Deleting the extracted program directory does not delete user data. Use **Data import and recovery** in the app for restore operations; do not merge `defaults/` folders by hand.

## Local integrations

API documentation is available from the desktop menu and uses the current local instance. Its OpenAPI specification contains only routes registered by the desktop profile.

Browser and Burp integration is disabled by default. Enable **Local plugin integration** explicitly, then choose **Use Desktop** in browser extension 0.4.0 or Burp extension 1.1.0. Discovery returns only a short-lived `http://127.0.0.1:<port>` endpoint and instance metadata. Enter the local administrator password and choose **Validate**; discovery never supplies a password or session token.

## Scope and data boundary

The desktop profile includes the local single-administrator workspace: conversations and Agents, HITL, tools/MCP, workflows, projects, assets, vulnerabilities, tasks, knowledge, roles, Skills, managed files, monitoring, audit, settings, and local API/plugin integration.

Local Terminal, WebShell, C2, robot connectors, platform multi-user RBAC, and remote-service mode are not desktop features. Their navigation, routes, services, dedicated assets, and OpenAPI examples are excluded from the portable candidate. The shared standalone server keeps its existing behavior.

## Verification and limitations

Every CI candidate verifies safe archive paths; application/core/native-host architecture; the resource manifest; excluded and sensitive content; a CycloneDX SBOM; SHA-256 checksums; backup/restore; an R1 0.1.0 state upgrade; program-directory replacement; and data/configuration retention.

The verified R2 artifacts were produced by [portable and plugin run 30689534423](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30689534423) from commit `8b8ec953e9c168be865084f72fa75ff8fd0b554a`. Windows x64, macOS arm64, macOS x64, and the browser/Burp integration artifact all passed. The same commit also passed the complete [three-platform desktop lifecycle run 30689534440](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30689534440). The artifacts are retained through August 8, 2026.

These ZIPs are unsigned development candidates:

- Windows antivirus may scan the Go sidecars on first launch, and missing WebView2 prevents the UI from opening.
- macOS Gatekeeper may block an unsigned, unnotarized app.
- Do not disable operating-system protections as an installation step. Public stable distribution still requires Windows signing and Apple Developer ID signing/notarization.

When startup fails, use the recovery page to open the fixed data or log directory. Keep the old data directory and recovery points until the new version has started and essential projects have been checked.
