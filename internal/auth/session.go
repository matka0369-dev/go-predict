// Package auth validates the same httpOnly session cookie core-service
// issues, directly against the shared sessions/users tables — this service
// has no separate login of its own and never will; a session is only ever
// created by core-service's /auth/login. Mirrors
// core-service/src/auth/guards/session-auth.guard.ts and
// session-token.util.ts exactly (same cookie name, same SHA-256 hash) so a
// cookie minted by one service is valid on the other.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID          string
	Email       string
	Username    string
	AccountType string
	AgentID     *string
	IsActive    bool
}

type contextKey struct{}

var userContextKey = contextKey{}

func baseCookieName() string {
	if name := os.Getenv("SESSION_COOKIE_NAME"); name != "" {
		return name
	}
	return "predictsim_sid"
}

// portalIDs mirrors core-service's session-cookie.constants.ts. Only this
// service's own caller (the Player portal) will realistically appear, but the
// full set is kept so the two implementations can be diffed against each
// other rather than reasoned about separately.
var portalIDs = map[string]bool{
	"platform-admin": true, "admin": true, "agent": true, "player": true,
}

// cookieNameFor namespaces the session cookie to the calling portal, because
// cookies are scoped by host and ignore the port — one name means all four
// portals share a single session slot. An absent or unrecognised header falls
// back to the base name, keeping non-browser callers working.
func cookieNameFor(r *http.Request) string {
	portal := r.Header.Get("X-Portal")
	if portalIDs[portal] {
		return baseCookieName() + "_" + strings.ReplaceAll(portal, "-", "_")
	}
	return baseCookieName()
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Middleware rejects any request without a valid, unexpired, unrevoked
// session, and attaches the authenticated User to the request context.
// Doesn't check is_active here — an individual handler decides whether a
// disabled account's request should be rejected differently than a missing
// one; this layer only answers "is this really an active session."
func Middleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(cookieNameFor(r))
			if err != nil || cookie.Value == "" {
				http.Error(w, "Not authenticated", http.StatusUnauthorized)
				return
			}

			tokenHash := hashToken(cookie.Value)

			var u User
			err = pool.QueryRow(r.Context(), `
				SELECT u.id, u.email, u.username, u.account_type, u.agent_id, u.is_active
				FROM sessions s
				JOIN users u ON u.id = s.user_id
				WHERE s.token_hash = $1
				  AND s.revoked_at IS NULL
				  AND s.expires_at > now()
			`, tokenHash).Scan(&u.ID, &u.Email, &u.Username, &u.AccountType, &u.AgentID, &u.IsActive)

			if err != nil {
				http.Error(w, "Not authenticated", http.StatusUnauthorized)
				return
			}

			if !u.IsActive {
				http.Error(w, "Account disabled", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey, u)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext panics if called outside Middleware — every handler on this
// service's authenticated routes is expected to be wrapped by it, the same
// way core-service's handlers assume SessionAuthGuard already ran.
func FromContext(ctx context.Context) User {
	u, ok := ctx.Value(userContextKey).(User)
	if !ok {
		panic("auth.FromContext called without auth.Middleware in the chain")
	}
	return u
}
