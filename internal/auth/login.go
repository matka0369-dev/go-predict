package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"predictsim/prediction-service/internal/db"
)

// SessionTTL and SessionRenewAfter mirror core-service's
// session-token.util.ts exactly — a session either service creates has to
// behave identically to one the other created, since both read the same
// sessions table and neither knows which of them wrote a given row.
const (
	SessionTTL        = 90 * 24 * time.Hour
	SessionRenewAfter = 24 * time.Hour
)

func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func isProd() bool {
	return os.Getenv("NODE_ENV") == "production"
}

// authenticatedUser is this service's version of core-service's
// AuthenticatedUser (auth.types.ts) — same field names/shape the Player
// frontend's AuthUser type already expects, since it was written against
// core-service's /auth/* responses.
type authenticatedUser struct {
	ID          string   `json:"id"`
	Email       string   `json:"email"`
	Username    string   `json:"username"`
	AccountType string   `json:"accountType"`
	AgentID     *string  `json:"agentId"`
	Permissions []string `json:"permissions"`
}

// permissionsFor mirrors auth.service.ts's toPermissionKeys — a flattened,
// deduplicated set of permission keys across every role a user holds. Empty
// for a native Player with no roles, which is the common case this
// service's only caller actually hits.
func permissionsFor(pool *pgxpool.Pool, ctx context.Context, userID string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT p.key
		FROM user_roles ur
		JOIN role_permissions rp ON rp.role_id = ur.role_id
		JOIN permissions p ON p.id = rp.permission_id
		WHERE ur.user_id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

type loginRequestBody struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

// Login is this service's only unauthenticated route. Mirrors
// AuthService.login + AuthController.login: identifier matched against
// email OR username (both unique), bcrypt-compared against password_hash
// (Go's x/crypto/bcrypt reads the same $2b$ hashes Node's bcrypt package
// wrote, so no rehash is needed), a fresh sessions row, and the same
// portal-namespaced cookie core-service would have set.
func Login(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body loginRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.Identifier) < 3 || len(body.Password) < 8 {
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}

		var (
			userID, email, username, accountType, passwordHash string
			agentID                                            *string
			isActive                                           bool
		)
		err := pool.QueryRow(r.Context(), `
			SELECT id, email, username, account_type, agent_id, is_active, password_hash
			FROM users
			WHERE email = $1 OR username = $1
		`, body.Identifier).Scan(&userID, &email, &username, &accountType, &agentID, &isActive, &passwordHash)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "Failed to sign in", http.StatusInternalServerError)
				return
			}
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}
		if !isActive || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)) != nil {
			http.Error(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}

		rawToken, err := generateSessionToken()
		if err != nil {
			http.Error(w, "Failed to sign in", http.StatusInternalServerError)
			return
		}

		var userAgent *string
		if ua := r.Header.Get("User-Agent"); ua != "" {
			userAgent = &ua
		}
		var ipAddress *string
		if ip := r.RemoteAddr; ip != "" {
			ipAddress = &ip
		}

		_, err = pool.Exec(r.Context(), `
			INSERT INTO sessions (id, user_id, token_hash, ip_address, user_agent, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, db.NewUUID(), userID, hashToken(rawToken), ipAddress, userAgent, time.Now().Add(SessionTTL))
		if err != nil {
			http.Error(w, "Failed to sign in", http.StatusInternalServerError)
			return
		}

		permissions, err := permissionsFor(pool, r.Context(), userID)
		if err != nil {
			http.Error(w, "Failed to sign in", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     cookieNameFor(r),
			Value:    rawToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   isProd(),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(SessionTTL.Seconds()),
		})

		writeJSON(w, http.StatusOK, map[string]any{
			"user": authenticatedUser{
				ID: userID, Email: email, Username: username, AccountType: accountType,
				AgentID: agentID, Permissions: permissions,
			},
		})
	}
}

// Logout mirrors AuthController.logout — revoke the current session (if the
// cookie names one still live), then clear the cookie regardless.
func Logout(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieNameFor(r))
		if err == nil && cookie.Value != "" {
			_, _ = pool.Exec(r.Context(), `
				UPDATE sessions SET revoked_at = now()
				WHERE token_hash = $1 AND revoked_at IS NULL
			`, hashToken(cookie.Value))
		}

		http.SetCookie(w, &http.Cookie{
			Name:     cookieNameFor(r),
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   isProd(),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	}
}

// Me mirrors AuthController.me — just re-serves what Middleware already
// authenticated, plus the permissions list (not carried on the context
// User, since nothing else on this service needs it).
func Me(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())
		permissions, err := permissionsFor(pool, r.Context(), u.ID)
		if err != nil {
			http.Error(w, "Failed to load account", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user": authenticatedUser{
				ID: u.ID, Email: u.Email, Username: u.Username, AccountType: u.AccountType,
				AgentID: u.AgentID, Permissions: permissions,
			},
		})
	}
}

type sessionInfo struct {
	ID        string  `json:"id"`
	IPAddress *string `json:"ipAddress"`
	UserAgent *string `json:"userAgent"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt string  `json:"expiresAt"`
	IsCurrent bool    `json:"isCurrent"`
}

// ListSessions mirrors AuthService.listSessions — every live session for
// the caller, flagging which one is the request's own.
func ListSessions(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())

		var currentHash string
		if cookie, err := r.Cookie(cookieNameFor(r)); err == nil && cookie.Value != "" {
			currentHash = hashToken(cookie.Value)
		}

		rows, err := pool.Query(r.Context(), `
			SELECT id, ip_address, user_agent, created_at, expires_at, token_hash
			FROM sessions
			WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
			ORDER BY created_at DESC
		`, u.ID)
		if err != nil {
			http.Error(w, "Failed to load sessions", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := []sessionInfo{}
		for rows.Next() {
			var (
				id, tokenHash        string
				ipAddress, userAgent *string
				createdAt, expiresAt time.Time
			)
			if err := rows.Scan(&id, &ipAddress, &userAgent, &createdAt, &expiresAt, &tokenHash); err != nil {
				http.Error(w, "Failed to load sessions", http.StatusInternalServerError)
				return
			}
			out = append(out, sessionInfo{
				ID: id, IPAddress: ipAddress, UserAgent: userAgent,
				CreatedAt: createdAt.Format(time.RFC3339), ExpiresAt: expiresAt.Format(time.RFC3339),
				IsCurrent: currentHash != "" && tokenHash == currentHash,
			})
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "Failed to load sessions", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, out)
	}
}

// RevokeSession mirrors AuthService.revokeSession — scoped to the caller's
// own sessions so a guessed id can never revoke someone else's.
func RevokeSession(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())
		id := chi.URLParam(r, "id")

		var owner string
		err := pool.QueryRow(r.Context(), `SELECT user_id FROM sessions WHERE id = $1`, id).Scan(&owner)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "Session not found", http.StatusNotFound)
				return
			}
			http.Error(w, "Failed to revoke session", http.StatusInternalServerError)
			return
		}
		if owner != u.ID {
			http.Error(w, "Session not found", http.StatusNotFound)
			return
		}

		_, err = pool.Exec(r.Context(), `
			UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL
		`, id)
		if err != nil {
			http.Error(w, "Failed to revoke session", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	}
}

// RevokeOtherSessions mirrors AuthService.revokeAllSessions — "log out
// everywhere else," keeping the session making this request alive.
func RevokeOtherSessions(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())

		var exceptHash string
		if cookie, err := r.Cookie(cookieNameFor(r)); err == nil && cookie.Value != "" {
			exceptHash = hashToken(cookie.Value)
		}

		_, err := pool.Exec(r.Context(), `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL AND token_hash != $2
		`, u.ID, exceptHash)
		if err != nil {
			http.Error(w, "Failed to revoke sessions", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	}
}
