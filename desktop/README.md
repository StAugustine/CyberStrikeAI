# CyberStrikeAI Desktop

This directory contains the Tauri v2 shell for the local CyberStrikeAI desktop client. The shell builds `cmd/desktop-core` as the `cyberstrike-core` sidecar and `cmd/desktop-native-host` as the narrow browser discovery host, resolves all writable platform directories to absolute paths, and passes only those paths plus the application version to the core.

The sidecar reports versioned `BOOTSTRAP_REQUIRED` and `READY` messages on stdout. On a fresh profile, a separate local bootstrap window is the only window permitted to submit the initial administrator password. The password is sent as a versioned JSON line over inherited stdin; it is not placed in process arguments, environment variables, URLs, configuration, or logs. After `READY`, the main WebView is restricted to the exact random `127.0.0.1` origin reported by the core.

From this directory:

```sh
npm install --ignore-scripts
npm run desktop:dev
```

Development mode stages bundled defaults and an isolated disposable profile under the repository `.tmp/` directory. Use `npm run desktop:build` for an unbundled debug build. A pinned Rust toolchain, the platform's native build prerequisites, a CGO-capable Go toolchain, Node.js, and npm are required.

After an unbundled debug build, the native integration smokes are:

```sh
npm run desktop:smoke
npm run desktop:smoke:single-instance
npm run desktop:smoke:failures
```

The lifecycle smoke creates an isolated profile under the repository `.tmp/` directory, completes first-launch bootstrap through the same stdin protocol, reaches the real core health gate, shuts down cleanly, and verifies that the test password was not persisted. The failure smoke verifies that invalid bundled resources fail closed with a non-zero application exit.

Local browser/Burp integration is off by default. Enabling it from the desktop user menu registers the pinned Chrome/Edge native-messaging host and publishes only a private, short-lived loopback endpoint. Discovery never contains authentication material; plugins must still log in through the existing local administrator API. Disabling integration, losing the core, or restarting the desktop removes the discovery file.
