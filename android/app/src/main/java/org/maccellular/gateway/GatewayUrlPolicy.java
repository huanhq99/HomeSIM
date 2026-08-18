package org.maccellular.gateway;

import java.net.URI;
import java.net.URISyntaxException;
import java.util.Locale;
import java.util.Objects;
import java.util.regex.Pattern;

/** Exact-origin policy shared by build-time tests and the WebView boundary. */
final class GatewayUrlPolicy {
    private static final Pattern TAILSCALE_HOST = Pattern.compile(
            "[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" +
                    "(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\\.ts\\.net");

    private final URI root;
    private final String host;

    GatewayUrlPolicy(String configuredUrl) {
        Objects.requireNonNull(configuredUrl, "configuredUrl");
        this.root = parse(configuredUrl);
        this.host = canonicalHost(root);
        if (!isExactHttpsOrigin(root) || !isTailnetHost(host) ||
                !"/remote/".equals(root.getRawPath()) || root.getRawQuery() != null ||
                root.getRawFragment() != null) {
            throw new IllegalArgumentException(
                    "Gateway URL must be the exact https://<host>.ts.net/remote/ root");
        }
    }

    String rootUrl() {
        return root.toASCIIString();
    }

    boolean isAllowedNavigation(String candidate) {
        URI uri = parseOrNull(candidate);
        return uri != null && hasExpectedOrigin(uri) && "/remote/".equals(uri.getRawPath()) &&
                uri.getRawQuery() == null && uri.getRawFragment() == null;
    }

    boolean isAllowedResource(String candidate) {
        URI uri = parseOrNull(candidate);
        if (uri == null || !hasExpectedOrigin(uri) || uri.getRawFragment() != null) {
            return false;
        }
        String path = uri.getRawPath();
        if (path == null || path.indexOf('%') >= 0 || path.indexOf('\\') >= 0 || path.contains("//") ||
                path.contains("/./") || path.contains("/../") || path.endsWith("/.") || path.endsWith("/..")) {
            return false;
        }
        return path.startsWith("/remote/") || path.startsWith("/api/remote/v1/") ||
                path.startsWith("/api/remote/v2/voice/");
    }

    boolean isAllowedPermissionOrigin(String candidate) {
        URI uri = parseOrNull(candidate);
        if (uri == null || !hasExpectedOrigin(uri) || uri.getRawQuery() != null ||
                uri.getRawFragment() != null) {
            return false;
        }
        String path = uri.getRawPath();
        return path == null || path.isEmpty() || "/".equals(path);
    }

    private boolean hasExpectedOrigin(URI uri) {
        return isExactHttpsOrigin(uri) && host.equals(canonicalHost(uri));
    }

    private static boolean isExactHttpsOrigin(URI uri) {
        return "https".equals(uri.getScheme()) && uri.getPort() == -1 && uri.getUserInfo() == null;
    }

    private static boolean isTailnetHost(String value) {
        return value.length() > ".ts.net".length() && TAILSCALE_HOST.matcher(value).matches();
    }

    private static String canonicalHost(URI uri) {
        return uri.getHost() == null ? "" : uri.getHost().toLowerCase(Locale.ROOT);
    }

    private static URI parse(String value) {
        try {
            return new URI(value);
        } catch (URISyntaxException error) {
            throw new IllegalArgumentException("Gateway URL is not a valid URI", error);
        }
    }

    private static URI parseOrNull(String value) {
        if (value == null || value.isEmpty()) {
            return null;
        }
        try {
            return new URI(value);
        } catch (URISyntaxException error) {
            return null;
        }
    }
}
