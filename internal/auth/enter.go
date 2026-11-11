package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"predictsim/prediction-service/internal/db"
)

// Sign-in for Players who never type a password: they redeem a one-time link
// an Agent issued (Enter), which enrolls their phone and sets their PIN, and
// thereafter reopen the app with phone-key + PIN (PinLogin). The links are
// issued by core-service (PlayerAccessService) — this service only redeems
// them — and both read the same login_tokens / player_devices tables.
//
// Error bodies here are stable machine codes, not prose ("wrong_pin:3",
// "locked"): the player app is shown in six languages to people who may not
// read well, so it maps these to its own wording and icons.

// maxPinAttempts wrong PINs revoke the phone; the Agent then issues a new
// link. Counted only after the phone's key has been verified, so a stranger
// who merely knows a username can neither sign in nor use up the attempts.
const maxPinAttempts = 5

var (
	pinRe      = regexp.MustCompile(`^[0-9]{4}$`)
	hexTokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type enterRequestBody struct {
	Token string `json:"token"`
	Pin   string `json:"pin"`
}

// Enter redeems a sign-in link. Consuming the link, enrolling the phone and
// creating the session happen in one transaction, so a failure anywhere
// leaves the link unspent instead of burning it for nothing.
func Enter(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body enterRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad_request", http.StatusBadRequest)
			return
		}
		if !pinRe.MatchString(body.Pin) {
			http.Error(w, "bad_pin", http.StatusBadRequest)
			return
		}
		if !hexTokenRe.MatchString(body.Token) {
			http.Error(w, "link_invalid", http.StatusUnauthorized)
			return
		}

		tx, err := pool.Begin(r.Context())
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(r.Context())

		// The conditional UPDATE is both the single-use guard and the expiry
		// check: of two simultaneous redemptions exactly one gets a row back.
		var userID string
		err = tx.QueryRow(r.Context(), `
			UPDATE login_tokens SET used_at = now()
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING user_id
		`, hashToken(body.Token)).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "link_invalid", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		var u User
		err = tx.QueryRow(r.Context(), `
			SELECT id, email, username, account_type, agent_id, is_active FROM users WHERE id = $1
		`, userID).Scan(&u.ID, &u.Email, &u.Username, &u.AccountType, &u.AgentID, &u.IsActive)
		if err != nil || !u.IsActive || u.AccountType != "PLAYER" {
			http.Error(w, "link_invalid", http.StatusUnauthorized)
			return
		}

		deviceKey, err := generateSessionToken()
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		pinHash, err := bcrypt.GenerateFromPassword([]byte(body.Pin), 10)
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		deviceID := db.NewUUID()
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO player_devices (id, user_id, key_hash, pin_hash, last_used_at)
			VALUES ($1, $2, $3, $4, now())
		`, deviceID, userID, hashToken(deviceKey), string(pinHash)); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		rawToken, err := insertSession(r, tx, userID)
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		setSessionCookie(w, r, rawToken)
		writeJSON(w, http.StatusOK, map[string]any{
			"user": authenticatedUser{
				ID: u.ID, Email: u.Email, Username: u.Username, AccountType: u.AccountType,
				AgentID: u.AgentID, Permissions: []string{},
			},
			"deviceId":  deviceID,
			"deviceKey": deviceKey,
		})
	}
}

type pinLoginRequestBody struct {
	DeviceID  string `json:"deviceId"`
	DeviceKey string `json:"deviceKey"`
	Pin       string `json:"pin"`
}

// PinLogin signs a Player back in from an enrolled phone.
func PinLogin(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body pinLoginRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad_request", http.StatusBadRequest)
			return
		}
		if !pinRe.MatchString(body.Pin) {
			http.Error(w, "bad_pin", http.StatusBadRequest)
			return
		}
		if !hexTokenRe.MatchString(body.DeviceKey) || len(body.DeviceID) < 8 || len(body.DeviceID) > 64 {
			http.Error(w, "unknown_device", http.StatusUnauthorized)
			return
		}

		var (
			userID, keyHash, pinHash string
			failed                   int
			revoked, active          bool
			accountType              string
		)
		err := pool.QueryRow(r.Context(), `
			SELECT d.user_id, d.key_hash, d.pin_hash, d.failed_attempts,
			       d.revoked_at IS NOT NULL, u.is_active, u.account_type
			FROM player_devices d
			JOIN users u ON u.id = d.user_id
			WHERE d.id = $1
		`, body.DeviceID).Scan(&userID, &keyHash, &pinHash, &failed, &revoked, &active, &accountType)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "unknown_device", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		// Key first and before anything attempt-related: without this phone's
		// key the request learns nothing and costs the real owner nothing.
		if subtle.ConstantTimeCompare([]byte(hashToken(body.DeviceKey)), []byte(keyHash)) != 1 {
			http.Error(w, "unknown_device", http.StatusUnauthorized)
			return
		}
		if revoked {
			http.Error(w, "locked", http.StatusForbidden)
			return
		}
		if !active || accountType != "PLAYER" {
			http.Error(w, "account_disabled", http.StatusForbidden)
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(pinHash), []byte(body.Pin)) != nil {
			// One statement so concurrent guesses can't each read the same
			// count and slip extra attempts past the limit.
			var attempts int
			err := pool.QueryRow(r.Context(), `
				UPDATE player_devices
				SET failed_attempts = failed_attempts + 1,
				    revoked_at = CASE WHEN failed_attempts + 1 >= $2 THEN now() ELSE revoked_at END
				WHERE id = $1
				RETURNING failed_attempts
			`, body.DeviceID, maxPinAttempts).Scan(&attempts)
			if err != nil {
				http.Error(w, "server_error", http.StatusInternalServerError)
				return
			}
			if attempts >= maxPinAttempts {
				http.Error(w, "locked", http.StatusForbidden)
				return
			}
			http.Error(w, fmt.Sprintf("wrong_pin:%d", maxPinAttempts-attempts), http.StatusUnauthorized)
			return
		}

		if _, err := pool.Exec(r.Context(), `
			UPDATE player_devices SET failed_attempts = 0, last_used_at = now() WHERE id = $1
		`, body.DeviceID); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		var u User
		if err := pool.QueryRow(r.Context(), `
			SELECT id, email, username, account_type, agent_id, is_active FROM users WHERE id = $1
		`, userID).Scan(&u.ID, &u.Email, &u.Username, &u.AccountType, &u.AgentID, &u.IsActive); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		rawToken, err := insertSession(r, pool, userID)
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, r, rawToken)
		writeJSON(w, http.StatusOK, map[string]any{
			"user": authenticatedUser{
				ID: u.ID, Email: u.Email, Username: u.Username, AccountType: u.AccountType,
				AgentID: u.AgentID, Permissions: []string{},
			},
		})
	}
}
