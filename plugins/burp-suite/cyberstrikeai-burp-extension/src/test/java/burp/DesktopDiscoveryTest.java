package burp;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.attribute.PosixFilePermission;
import java.time.Instant;
import java.util.EnumSet;

public final class DesktopDiscoveryTest {
    private static final long NOW = 2_000_000_000L;

    private DesktopDiscoveryTest() {}

    public static void main(String[] args) throws Exception {
        Path testRoot = Paths.get(System.getProperty("cyberstrike.test.tmp", ""));
        if (!testRoot.isAbsolute()) {
            throw new AssertionError("cyberstrike.test.tmp must be absolute");
        }
        Files.createDirectories(testRoot);

        acceptsShortLivedLoopbackMetadata(testRoot.resolve("valid.json"));
        rejectsCredentialAndUnknownFields(testRoot.resolve("credential.json"));
        rejectsExpiredAndRemoteMetadata(testRoot.resolve("invalid.json"));
        resolvesSupportedPlatformPaths(testRoot);
    }

    private static void acceptsShortLivedLoopbackMetadata(Path path) throws Exception {
        writePrivate(path, discovery("http://127.0.0.1:43123", NOW - 5, NOW + 85, ""));
        DesktopDiscovery.Endpoint endpoint = DesktopDiscovery.load(path, Instant.ofEpochSecond(NOW));
        check("127.0.0.1".equals(endpoint.host), "loopback host");
        check(endpoint.port == 43123, "loopback port");
        check("0.1.0".equals(endpoint.appVersion), "app version");
        check("desktop-instance-1234".equals(endpoint.instanceId), "instance id");
    }

    private static void rejectsCredentialAndUnknownFields(Path path) throws Exception {
        writePrivate(path, discovery("http://127.0.0.1:43123", NOW - 5, NOW + 85, ",\"token\":\"secret\""));
        expectFailure(path, "credential field");

        writePrivate(path, discovery("http://127.0.0.1:43123", NOW - 5, NOW + 85, ",\"extra\":true"));
        expectFailure(path, "unknown field");
    }

    private static void rejectsExpiredAndRemoteMetadata(Path path) throws Exception {
        writePrivate(path, discovery("http://127.0.0.1:43123", NOW - 100, NOW - 1, ""));
        expectFailure(path, "expired metadata");

        writePrivate(path, discovery("http://192.168.1.5:43123", NOW - 5, NOW + 85, ""));
        expectFailure(path, "remote endpoint");
    }

    private static void resolvesSupportedPlatformPaths(Path testRoot) throws Exception {
        Path windows = DesktopDiscovery.defaultPath("Windows 11", testRoot.toString(), testRoot.toString());
        check(windows.equals(testRoot.resolve("com.cyberstrikeai.desktop").resolve("plugin-discovery.json")),
                "Windows discovery path");

        Path mac = DesktopDiscovery.defaultPath("Mac OS X", null, testRoot.toString());
        check(mac.equals(testRoot.resolve("Library").resolve("Application Support")
                        .resolve("com.cyberstrikeai.desktop").resolve("plugin-discovery.json")),
                "macOS discovery path");
    }

    private static String discovery(String baseUrl, long issuedAt, long expiresAt, String extra) {
        return "{"
                + "\"schema_version\":1,"
                + "\"instance_id\":\"desktop-instance-1234\","
                + "\"base_url\":\"" + baseUrl + "\","
                + "\"app_version\":\"0.1.0\","
                + "\"issued_at_unix\":" + issuedAt + ","
                + "\"expires_at_unix\":" + expiresAt
                + extra + "}";
    }

    private static void writePrivate(Path path, String content) throws IOException {
        Files.write(path, content.getBytes(StandardCharsets.UTF_8));
        try {
            Files.setPosixFilePermissions(path, EnumSet.of(PosixFilePermission.OWNER_READ, PosixFilePermission.OWNER_WRITE));
        } catch (UnsupportedOperationException ignored) {
            // Windows does not expose POSIX permissions.
        }
    }

    private static void expectFailure(Path path, String label) throws Exception {
        try {
            DesktopDiscovery.load(path, Instant.ofEpochSecond(NOW));
            throw new AssertionError("Expected failure for " + label);
        } catch (IOException expected) {
            // Expected.
        }
    }

    private static void check(boolean condition, String label) {
        if (!condition) {
            throw new AssertionError("Failed: " + label);
        }
    }
}
