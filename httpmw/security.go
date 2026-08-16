package httpmw

import (
	"net/http"
	"strconv"
)

// DefaultContentSecurityPolicy is a policy for a JSON API: it never returns
// documents, so nothing needs to load and nothing needs to frame it.
const DefaultContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"

// SecurityHeadersConfig configures SecurityHeaders. The zero value is a sound
// default for a JSON API behind a TLS-terminating proxy.
type SecurityHeadersConfig struct {
	// HSTSMaxAgeSeconds, when greater than zero, sets Strict-Transport-Security.
	// Leave it zero when TLS terminates at a proxy that already sets the header
	// — two sources for one policy is how a stale max-age outlives the config
	// that was supposed to have replaced it.
	HSTSMaxAgeSeconds int
	// HSTSIncludeSubdomains adds includeSubDomains. Only safe once every
	// subdomain genuinely serves HTTPS; it is not quickly reversible, because
	// browsers cache the directive for the full max-age.
	HSTSIncludeSubdomains bool
	// ContentSecurityPolicy overrides DefaultContentSecurityPolicy.
	ContentSecurityPolicy string
	// CrossOriginResourcePolicy, when set, sets Cross-Origin-Resource-Policy.
	// Left empty by default: "same-origin" is right for an API served from the
	// same origin as its web client, and breaks one served cross-origin, and
	// this package cannot know which deployment it is in.
	CrossOriginResourcePolicy string
}

// SecurityHeaders sets the response headers that are correct for a JSON API
// regardless of route.
//
// This is thirty lines rather than a dependency on something like
// unrolled/secure deliberately: that package's real value is in the pieces
// that belong at the edge — TLS redirects, host allowlisting — and for a JSON
// API the correct headers are a short static list. Revisit if this file ever
// starts growing conditionals.
//
// Notably absent: X-XSS-Protection. The legacy auditor it enabled is gone from
// modern browsers and introduced vulnerabilities of its own; the header is
// deprecated, and setting it to anything but 0 is now a mild liability.
func SecurityHeaders(cfg SecurityHeadersConfig) func(http.Handler) http.Handler {
	csp := cfg.ContentSecurityPolicy
	if csp == "" {
		csp = DefaultContentSecurityPolicy
	}

	var hsts string
	if cfg.HSTSMaxAgeSeconds > 0 {
		hsts = "max-age=" + strconv.Itoa(cfg.HSTSMaxAgeSeconds)
		if cfg.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			// Stops a browser from second-guessing our Content-Type. Without
			// it, a JSON response an attacker controls can be coaxed into
			// executing as script.
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			if hsts != "" {
				h.Set("Strict-Transport-Security", hsts)
			}
			if cfg.CrossOriginResourcePolicy != "" {
				h.Set("Cross-Origin-Resource-Policy", cfg.CrossOriginResourcePolicy)
			}
			next.ServeHTTP(w, r)
		})
	}
}
