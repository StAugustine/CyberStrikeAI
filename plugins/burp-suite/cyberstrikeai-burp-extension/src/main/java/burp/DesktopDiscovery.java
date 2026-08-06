package burp;

import java.io.IOException;
import java.net.URI;
import java.net.URISyntaxException;
import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.attribute.PosixFilePermission;
import java.time.Instant;
import java.util.HashMap;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;

final class DesktopDiscovery {
    private static final int SCHEMA_VERSION = 1;
    private static final long MAXIMUM_BYTES = 16 * 1024;
    private static final long MAXIMUM_TTL_SECONDS = 120;
    private static final Pattern INSTANCE_ID = Pattern.compile("[A-Za-z0-9._-]{16,128}");
    private static final Set<String> EXPECTED_KEYS = Set.of(
            "schema_version", "instance_id", "base_url", "app_version", "issued_at_unix", "expires_at_unix");

    private DesktopDiscovery() {}

    static final class Endpoint {
        final String baseUrl;
        final String host;
        final int port;
        final String appVersion;
        final String instanceId;

        Endpoint(String baseUrl, String host, int port, String appVersion, String instanceId) {
            this.baseUrl = baseUrl;
            this.host = host;
            this.port = port;
            this.appVersion = appVersion;
            this.instanceId = instanceId;
        }
    }

    static Endpoint loadDefault() throws IOException {
        return load(defaultPath(), Instant.now());
    }

    static Endpoint load(Path path, Instant now) throws IOException {
        if (path == null || !path.isAbsolute()) {
            throw new IOException("Desktop discovery path must be absolute");
        }
        if (Files.isSymbolicLink(path) || !Files.isRegularFile(path, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Desktop discovery is not a regular file");
        }
        long size = Files.size(path);
        if (size <= 0 || size > MAXIMUM_BYTES) {
            throw new IOException("Desktop discovery has an invalid size");
        }
        requirePrivatePermissions(path);

        byte[] data = Files.readAllBytes(path);
        if (data.length == 0 || data.length > MAXIMUM_BYTES) {
            throw new IOException("Desktop discovery has an invalid size");
        }
        String json = decodeUtf8(data);
        Map<String, Object> fields = new FlatObjectParser(json).parse();
        if (!fields.keySet().equals(EXPECTED_KEYS)) {
            throw new IOException("Desktop discovery contains unsupported fields");
        }

        long schemaVersion = requireLong(fields, "schema_version");
        String instanceId = requireString(fields, "instance_id");
        String baseUrl = requireString(fields, "base_url");
        String appVersion = requireString(fields, "app_version");
        long issuedAt = requireLong(fields, "issued_at_unix");
        long expiresAt = requireLong(fields, "expires_at_unix");

        if (schemaVersion != SCHEMA_VERSION) {
            throw new IOException("Desktop discovery schema is unsupported");
        }
        if (!INSTANCE_ID.matcher(instanceId).matches()) {
            throw new IOException("Desktop discovery instance is invalid");
        }
        if (appVersion.trim().isEmpty() || appVersion.length() > 64) {
            throw new IOException("Desktop discovery version is invalid");
        }
        if (issuedAt <= 0 || expiresAt <= issuedAt || expiresAt - issuedAt > MAXIMUM_TTL_SECONDS) {
            throw new IOException("Desktop discovery lifetime is invalid");
        }
        long nowEpoch = now.getEpochSecond();
        if (issuedAt > nowEpoch + 30) {
            throw new IOException("Desktop discovery was issued in the future");
        }
        if (expiresAt <= nowEpoch) {
            throw new IOException("Desktop discovery has expired");
        }

        URI endpoint = parseEndpoint(baseUrl);
        return new Endpoint(baseUrl, endpoint.getHost(), endpoint.getPort(), appVersion, instanceId);
    }

    static Path defaultPath() throws IOException {
        return defaultPath(
                System.getProperty("os.name", ""),
                System.getenv("APPDATA"),
                System.getProperty("user.home", ""));
    }

    static Path defaultPath(String osName, String appData, String userHome) throws IOException {
        String normalizedOS = osName == null ? "" : osName.toLowerCase();
        Path root;
        if (normalizedOS.contains("win")) {
            root = pathFromRequiredValue(appData, "Windows application data");
        } else {
            Path home = pathFromRequiredValue(userHome, "User home");
            root = normalizedOS.contains("mac")
                    ? home.resolve("Library").resolve("Application Support")
                    : home.resolve(".config");
        }
        return root.resolve("com.cyberstrikeai.desktop").resolve("plugin-discovery.json");
    }

    private static Path pathFromRequiredValue(String value, String label) throws IOException {
        if (value == null || value.trim().isEmpty()) {
            throw new IOException(label + " is unavailable");
        }
        Path path = Paths.get(value);
        if (!path.isAbsolute()) {
            throw new IOException(label + " is not absolute");
        }
        return path;
    }

    private static URI parseEndpoint(String baseUrl) throws IOException {
        try {
            URI uri = new URI(baseUrl.trim());
            String path = uri.getRawPath();
            if (!"http".equals(uri.getScheme()) || !"127.0.0.1".equals(uri.getHost())
                    || uri.getPort() < 1 || uri.getPort() > 65535 || uri.getRawUserInfo() != null
                    || (path != null && !path.isEmpty() && !"/".equals(path))
                    || uri.getRawQuery() != null || uri.getRawFragment() != null) {
                throw new IOException("Desktop discovery endpoint is invalid");
            }
            return uri;
        } catch (URISyntaxException e) {
            throw new IOException("Desktop discovery endpoint is invalid", e);
        }
    }

    private static void requirePrivatePermissions(Path path) throws IOException {
        try {
            Set<PosixFilePermission> permissions = Files.getPosixFilePermissions(path, LinkOption.NOFOLLOW_LINKS);
            for (PosixFilePermission permission : permissions) {
                if (permission.name().startsWith("GROUP_") || permission.name().startsWith("OTHERS_")) {
                    throw new IOException("Desktop discovery permissions are too broad");
                }
            }
        } catch (UnsupportedOperationException ignored) {
            // Windows ACL enforcement is owned by the desktop process that creates the file.
        }
    }

    private static String decodeUtf8(byte[] data) throws IOException {
        try {
            return StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(data))
                    .toString();
        } catch (CharacterCodingException e) {
            throw new IOException("Desktop discovery is invalid", e);
        }
    }

    private static String requireString(Map<String, Object> fields, String key) throws IOException {
        Object value = fields.get(key);
        if (!(value instanceof String)) {
            throw new IOException("Desktop discovery is invalid");
        }
        return (String) value;
    }

    private static long requireLong(Map<String, Object> fields, String key) throws IOException {
        Object value = fields.get(key);
        if (!(value instanceof Long)) {
            throw new IOException("Desktop discovery is invalid");
        }
        return (Long) value;
    }

    private static final class FlatObjectParser {
        private final String input;
        private int position;

        FlatObjectParser(String input) {
            this.input = input;
        }

        Map<String, Object> parse() throws IOException {
            Map<String, Object> fields = new HashMap<>();
            skipWhitespace();
            expect('{');
            skipWhitespace();
            if (consume('}')) {
                finish();
                return fields;
            }
            while (true) {
                String key = parseString();
                skipWhitespace();
                expect(':');
                skipWhitespace();
                Object value = peek() == '"' ? parseString() : parseLong();
                if (fields.put(key, value) != null) {
                    throw invalid();
                }
                skipWhitespace();
                if (consume('}')) {
                    finish();
                    return fields;
                }
                expect(',');
                skipWhitespace();
            }
        }

        private String parseString() throws IOException {
            expect('"');
            StringBuilder value = new StringBuilder();
            while (position < input.length()) {
                char character = input.charAt(position++);
                if (character == '"') {
                    return value.toString();
                }
                if (character < 0x20) {
                    throw invalid();
                }
                if (character != '\\') {
                    value.append(character);
                    continue;
                }
                if (position >= input.length()) {
                    throw invalid();
                }
                char escaped = input.charAt(position++);
                switch (escaped) {
                    case '"': value.append('"'); break;
                    case '\\': value.append('\\'); break;
                    case '/': value.append('/'); break;
                    case 'b': value.append('\b'); break;
                    case 'f': value.append('\f'); break;
                    case 'n': value.append('\n'); break;
                    case 'r': value.append('\r'); break;
                    case 't': value.append('\t'); break;
                    case 'u':
                        if (position + 4 > input.length()) {
                            throw invalid();
                        }
                        try {
                            value.append((char) Integer.parseInt(input.substring(position, position + 4), 16));
                        } catch (NumberFormatException e) {
                            throw invalid();
                        }
                        position += 4;
                        break;
                    default:
                        throw invalid();
                }
            }
            throw invalid();
        }

        private long parseLong() throws IOException {
            int start = position;
            consume('-');
            int digits = position;
            while (position < input.length() && Character.isDigit(input.charAt(position))) {
                position++;
            }
            if (digits == position) {
                throw invalid();
            }
            try {
                return Long.parseLong(input.substring(start, position));
            } catch (NumberFormatException e) {
                throw invalid();
            }
        }

        private void finish() throws IOException {
            skipWhitespace();
            if (position != input.length()) {
                throw invalid();
            }
        }

        private char peek() throws IOException {
            if (position >= input.length()) {
                throw invalid();
            }
            return input.charAt(position);
        }

        private void expect(char expected) throws IOException {
            if (!consume(expected)) {
                throw invalid();
            }
        }

        private boolean consume(char expected) {
            if (position < input.length() && input.charAt(position) == expected) {
                position++;
                return true;
            }
            return false;
        }

        private void skipWhitespace() {
            while (position < input.length()) {
                char character = input.charAt(position);
                if (character != ' ' && character != '\t' && character != '\r' && character != '\n') {
                    return;
                }
                position++;
            }
        }

        private IOException invalid() {
            return new IOException("Desktop discovery is invalid");
        }
    }
}
