// PicoClaw - Ultra-lightweight personal AI agent

package wire

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// InsecureHTTPEnv is the explicit opt-out from the TLS requirement. Without it
// (or a loopback host) a plain-http base_url is refused, because the bearer
// token would otherwise travel in clear text and could be replayed by anyone
// on the path.
const InsecureHTTPEnv = "A2A_INSECURE_HTTP"

// ValidateBaseURL checks the platform URL before a token is ever attached.
// Loopback hosts stay allowed so local development and tests keep working.
func ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if insecureHTTPAllowed() || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf(
			"base_url %q uses http: the bearer token would be sent in clear text; "+
				"use https, or set %s=1 to override for a trusted private network",
			raw, InsecureHTTPEnv,
		)
	default:
		return fmt.Errorf("base_url %q must use http or https", raw)
	}
}

func insecureHTTPAllowed() bool {
	v := strings.TrimSpace(os.Getenv(InsecureHTTPEnv))
	if v == "" {
		return false
	}
	ok, err := strconv.ParseBool(v)
	return err == nil && ok
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
