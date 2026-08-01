import { cp, lstat, mkdir, readFile, readdir, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments, requireReleaseTarget } from "./release-support.mjs";
import { binaryNamesForTarget } from "./build-config.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");

export async function packagePortable({
  root = repositoryDirectory,
  targetTriple,
  buildRoot,
  stageDirectory,
  outputDirectory,
  archiveRunner = runArchive,
}) {
  const releaseTarget = requireReleaseTarget(targetTriple);
  const packageJSON = JSON.parse(await readFile(path.join(root, "desktop", "package.json"), "utf8"));
  const tauriConfig = JSON.parse(
    await readFile(path.join(root, "desktop", "src-tauri", "tauri.conf.json"), "utf8"),
  );
  await prepareEmptyDirectory(stageDirectory, "portable stage");
  await prepareEmptyDirectory(outputDirectory, "portable output");

  const rootName = `CyberStrikeAI-Desktop-${packageJSON.version}-${releaseTarget.archiveLabel}`;
  const portableRoot = path.join(stageDirectory, rootName);
  await mkdir(portableRoot);
  await cp(path.join(root, "LICENSE"), path.join(portableRoot, "LICENSE"));
  await writeFile(path.join(portableRoot, "README.txt"), portableReadme(releaseTarget.portableKind), "utf8");

  if (releaseTarget.portableKind === "macos-app") {
    const appBundle = await findMacApplication(buildRoot);
    await cp(appBundle, path.join(portableRoot, path.basename(appBundle)), {
      recursive: true,
      preserveTimestamps: true,
      verbatimSymlinks: true,
    });
  } else {
    await stageWindowsPortable({ root, targetTriple, buildRoot, portableRoot, tauriConfig });
  }

  const archivePath = path.join(outputDirectory, `${rootName}-portable.zip`);
  await archiveRunner({ portableRoot, archivePath });
  const archiveInfo = await lstat(archivePath);
  if (!archiveInfo.isFile() || archiveInfo.size === 0) throw new Error("portable archive was not created");
  return { archivePath, portableRoot };
}

async function stageWindowsPortable({ root, targetTriple, buildRoot, portableRoot, tauriConfig }) {
  const binaryNames = binaryNamesForTarget(targetTriple, root);
  const executable = path.join(buildRoot, "cyberstrike-desktop.exe");
  await requireFile(executable, "Windows desktop executable");
  await cp(executable, path.join(portableRoot, "CyberStrikeAI Desktop.exe"));
  for (const name of [binaryNames.core, binaryNames.nativeHost]) {
    const sidecar = path.join(
      root,
      "desktop",
      "src-tauri",
      "binaries",
      `${name}-${targetTriple}.exe`,
    );
    await requireFile(sidecar, `Windows ${name} sidecar`);
    await cp(sidecar, path.join(portableRoot, `${name}.exe`));
  }

  const configDirectory = path.join(root, "desktop", "src-tauri");
  for (const [source, destination] of Object.entries(tauriConfig.bundle.resources)) {
    await cp(path.resolve(configDirectory, source), path.join(portableRoot, ...destination.split("/")), {
      recursive: true,
      preserveTimestamps: true,
      verbatimSymlinks: true,
    });
  }
}

async function findMacApplication(buildRoot) {
  const macOSBundleDirectory = path.join(buildRoot, "bundle", "macos");
  const entries = await readdir(macOSBundleDirectory, { withFileTypes: true });
  const applications = entries.filter((entry) => entry.isDirectory() && entry.name.endsWith(".app"));
  if (applications.length !== 1) {
    throw new Error(`expected one macOS application bundle, found ${applications.length}`);
  }
  return path.join(macOSBundleDirectory, applications[0].name);
}

async function prepareEmptyDirectory(directory, label) {
  await mkdir(directory, { recursive: true });
  if ((await readdir(directory)).length !== 0) throw new Error(`${label} directory must be empty: ${directory}`);
}

async function requireFile(filePath, label) {
  const info = await lstat(filePath).catch((error) => {
    if (error?.code === "ENOENT") throw new Error(`${label} is missing: ${filePath}`);
    throw error;
  });
  if (!info.isFile()) throw new Error(`${label} is not a file: ${filePath}`);
}

function portableReadme(kind) {
  const launch = kind === "macos-app"
    ? "Open the CyberStrikeAI Desktop.app bundle in this folder."
    : "Run CyberStrikeAI Desktop.exe in this folder. Microsoft Edge WebView2 Evergreen Runtime is required.";
  return [
    "CyberStrikeAI Desktop portable build",
    "",
    launch,
    "Keep all files in this folder together.",
    "Application data is stored in the operating system user-data directories, not in this folder.",
    "Deleting this folder removes the program but preserves user data for a later portable version.",
    "This unsigned candidate is for development validation only.",
    "",
  ].join("\n");
}

function runArchive({ portableRoot, archivePath }) {
  const parent = path.dirname(portableRoot);
  let result;
  if (process.platform === "darwin") {
    result = spawnSync(
      "/usr/bin/ditto",
      ["-c", "-k", "--sequesterRsrc", "--keepParent", portableRoot, archivePath],
      { cwd: parent, stdio: "inherit" },
    );
  } else {
    result = spawnSync(
      "powershell.exe",
      [
        "-NoLogo",
        "-NoProfile",
        "-NonInteractive",
        "-Command",
        "Add-Type -AssemblyName System.IO.Compression.FileSystem; [System.IO.Compression.ZipFile]::CreateFromDirectory($env:CYBERSTRIKE_ZIP_SOURCE, $env:CYBERSTRIKE_ZIP_DESTINATION, [System.IO.Compression.CompressionLevel]::Optimal, $false)",
      ],
      {
        cwd: parent,
        env: {
          ...process.env,
          CYBERSTRIKE_ZIP_SOURCE: parent,
          CYBERSTRIKE_ZIP_DESTINATION: archivePath,
        },
        stdio: "inherit",
      },
    );
  }
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`portable archive command failed with status ${result.status}`);
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const args = parseArguments(process.argv.slice(2), [
    "target",
    "build-root",
    "stage-directory",
    "output-directory",
  ]);
  const targetTriple = args.target || process.env.CYBERSTRIKE_DESKTOP_TARGET;
  const buildRoot = path.resolve(args["build-root"] || process.env.CYBERSTRIKE_RELEASE_BUILD_ROOT || "");
  const stageDirectory = path.resolve(
    args["stage-directory"] || process.env.CYBERSTRIKE_PORTABLE_STAGE || "",
  );
  const outputDirectory = path.resolve(
    args["output-directory"] || process.env.CYBERSTRIKE_RELEASE_OUTPUT || "",
  );
  if (!targetTriple || (!args["build-root"] && !process.env.CYBERSTRIKE_RELEASE_BUILD_ROOT)
    || (!args["stage-directory"] && !process.env.CYBERSTRIKE_PORTABLE_STAGE)
    || (!args["output-directory"] && !process.env.CYBERSTRIKE_RELEASE_OUTPUT)) {
    throw new Error("target, build root, stage directory and output directory are required");
  }
  const result = await packagePortable({ targetTriple, buildRoot, stageDirectory, outputDirectory });
  console.log(`created portable archive: ${result.archivePath}`);
}
