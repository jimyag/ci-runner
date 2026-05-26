package redact

import "net/url"

// DatabaseURL removes userinfo from database URLs while preserving location fields.
func DatabaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	if u.User != nil {
		u.User = url.User("xxxxx")
	}
	return u.String()
}
