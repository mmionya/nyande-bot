package logutil

import (
	"net/url"
	"strings"
)

// URL omits credentials, signed query parameters, and fragments.
func URL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "<invalid-url>"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.ForceQuery = false
	return u.String()
}
