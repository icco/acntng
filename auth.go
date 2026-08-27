package main

import (
	"crypto/subtle"
	"net/http"

	"github.com/icco/gutil/logging"
	"github.com/icco/gutil/render"
)

// sharedKeyHeader carries the secret Caddy injects on proxied requests.
const sharedKeyHeader = "X-Acntng-Key"

// requireSharedKey gates the report routes on a secret only Caddy knows.
//
// The portal protects the public route, but ~40 siblings on mist's shared
// caddy network can reach acntng:8080 directly -- including hoarder, which
// crawls user-submitted URLs through a chrome listening on 0.0.0.0:9222. The
// portal's injected X-WEBAUTH-USER is forgeable by anything that reaches the
// port, so only an unguessable secret distinguishes Caddy from a sibling.
func requireSharedKey(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			got := r.Header.Get(sharedKeyHeader)
			if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				log := logging.FromContext(r.Context())
				log.Warnw("rejected request without a valid shared key",
					"remote", r.RemoteAddr, "path", r.URL.Path)
				render.JSON(log, w, http.StatusForbidden,
					map[string]string{"error": "forbidden"})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
