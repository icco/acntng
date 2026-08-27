package main

import (
	"crypto/subtle"
	"net/http"

	"github.com/icco/gutil/logging"
	"github.com/icco/gutil/render"
)

// sharedKeyHeader carries the secret Caddy injects on proxied requests.
const sharedKeyHeader = "X-Acntng-Key"

// requireSharedKey gates the report routes on a secret that only Caddy knows.
//
// The Caddy auth portal in front of this service protects the public route,
// but the container also sits on mist's shared "caddy" docker network, where
// roughly forty sibling containers can reach acntng:8080 by name and skip the
// portal entirely. Two of those siblings make that reachable by an outsider:
// hoarder crawls arbitrary user-submitted bookmark URLs, and it drives them
// through a headless chrome whose DevTools port listens on 0.0.0.0:9222. A
// bookmark of http://acntng:8080/ would archive this account's loan balances.
//
// Checking the identity headers the portal injects (X-WEBAUTH-USER and
// friends) would not help: anything that can reach the port can forge them.
// Only a secret the network cannot guess distinguishes Caddy from a sibling.
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
