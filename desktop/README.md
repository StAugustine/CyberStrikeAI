# CyberStrikeAI Desktop D1 PoC

This directory contains the D1 protocol proof of concept only. It does not yet host the production application or migrate any business module.

The PoC fixes exact Tauri dependency versions, builds `cmd/desktop-poc-sidecar` as the `cyberstrike-go-poc` binary for the current Rust target, starts it as a packaged sidecar, accepts only a versioned `READY` message containing an explicit random `127.0.0.1` HTTP URL, and navigates the WebView to that origin. Closing the application requests graceful shutdown over stdin and force-kills the child only after a five-second timeout.

From this directory:

```sh
npm install --ignore-scripts
npm run poc:dev
```

Use `npm run poc:build` for an unbundled debug build. A Rust stable toolchain, the platform's native build prerequisites, Go, Node.js, and npm are required. D1 is not complete until this behavior has been built and run on Windows x64, macOS arm64, and macOS x64.

After an unbundled debug build, the automated native smokes are:

```sh
npm run poc:smoke
npm run poc:smoke:single-instance
npm run poc:smoke:failures
```

The normal smoke exits only after the WebView reports successful REST, EventSource, WebSocket and external-navigation rejection, and after Tauri has intercepted and cancelled a test download. Cancelling that download is intentional: the PoC must not write to the user's Downloads directory. The failure smoke expects exit code 1 for an unexpected sidecar crash and a non-zero exit after the five-second forced-shutdown fallback.
