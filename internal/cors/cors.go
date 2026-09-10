// Package cors mirrors core-service's main.ts CORS policy: any
// localhost/127.0.0.1 origin in dev (frontends move ports constantly),
// credentials-enabled CORS headers only for origins explicitly listed in
// CORS_ALLOWED_ORIGINS in prod. An unlisted origin is served with no CORS
// headers rather than rejected — the portals reach this service same-origin
// through Vercel /api/prediction/* rewrites, which forward the browser's
// Origin header. "*" is not permitted by browsers alongside credentials.
package cors

import (
	"net/http"
	"os"
	"regexp"
	"strings"
)

var localhostOrigin = regexp.MustCompile(`^https?://(localhost|127\.0\.0\.1)(:\d+)?$`)

func Middleware(next http.Handler) http.Handler {
	isProd := os.Getenv("NODE_ENV") == "production"
	allowlist := map[string]bool{}
	for o := range strings.SplitSeq(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			allowlist[o] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin == "" || (!isProd && localhostOrigin.MatchString(origin)) || allowlist[origin]

		if origin != "" && allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			// X-Portal namespaces the session cookie per portal — without it
			// here the browser drops the header at preflight and every
			// request falls back to the shared cookie slot. See
			// core-service session-cookie.constants.ts.
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Portal")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
