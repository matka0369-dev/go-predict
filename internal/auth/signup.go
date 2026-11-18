package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"predictsim/prediction-service/internal/db"
)

// Self-service registration: a person opens their Agent's app, gives a name,
// a phone number and a PIN, and has an account — no Agent step in between.
//
// What that does and doesn't establish:
//   - The Agent is whoever's invite code the app carries, so the new Player
//     lands in exactly the place an Agent-created one would: agent_id and
//     created_by_id both point at that Agent, and it reads that Agent's rate
//     card. The Agent still sets the odds; it just no longer sets people up.
//   - The phone number is NOT verified. There is no OTP — this is for people
//     who can't reliably read one — so it is contact information, not proof of
//     ownership. That is why a number already registered under the same Admin
//     is refused rather than "recovered": letting a second person claim it
//     would hand them someone else's account. The real owner goes through
//     their Agent (a new sign-in link), which also revokes the squatter's phone.
//   - A new account holds no tokens, so an unwanted one has nothing to take.
//     The abuse left is volume, which the limiter below caps.
//
// Error bodies are machine codes, like enter.go's.

var (
	nameRe = regexp.MustCompile(`^[\p{L}\p{M}][\p{L}\p{M} .'-]{1,39}$`)
	spaces = regexp.MustCompile(`\s+`)
)

type signupRequestBody struct {
	AgentCode string `json:"agentCode"`
	Name      string `json:"name"`
	Phone     string `json:"phone"`
	Pin       string `json:"pin"`
}

// normalizePhone returns digits-with-country-code, the same shape core-service
// stores for a Player's phone. A bare 10-digit number gets the default country
// code (India unless DEFAULT_COUNTRY_CODE says otherwise), so the player isn't
// asked to know what a country code is.
func normalizePhone(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if len(digits) == 10 {
		cc := os.Getenv("DEFAULT_COUNTRY_CODE")
		if cc == "" {
			cc = "91"
		}
		digits = cc + digits
	}
	if len(digits) < 11 || len(digits) > 15 {
		return "", false
	}
	return digits, true
}

// usernameBase makes a readable, unique-ish login name from the player's name
// and the last digits of their phone, e.g. "ramesh_4821". Agents see this in
// their player list, so it should tell them who someone is. Names in scripts
// that don't transliterate to ASCII fall back to "player".
func usernameBase(name, phone string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
		if b.Len() >= 14 {
			break
		}
	}
	if b.Len() == 0 {
		b.WriteString("player")
	}
	return b.String() + "_" + phone[len(phone)-4:]
}

// --- throttle -------------------------------------------------------------

// A small fixed-window counter. In-memory is deliberate: this service runs as
// one instance, and the limit is a speed bump against scripted sign-ups, not
// an accounting figure — losing it on a restart costs nothing. If the service
// is ever scaled out, each instance simply allows its own share.
type throttle struct {
	mu     sync.Mutex
	window time.Duration
	hits   map[string]*bucket
	lastGC time.Time
}

type bucket struct {
	count int
	reset time.Time
}

func newThrottle(window time.Duration) *throttle {
	return &throttle{window: window, hits: map[string]*bucket{}}
}

// allow reports whether key may act, counting this attempt.
func (t *throttle) allow(key string, limit int) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.lastGC) > t.window {
		for k, v := range t.hits {
			if now.After(v.reset) {
				delete(t.hits, k)
			}
		}
		t.lastGC = now
	}
	b := t.hits[key]
	if b == nil || now.After(b.reset) {
		b = &bucket{reset: now.Add(t.window)}
		t.hits[key] = b
	}
	b.count++
	return b.count <= limit
}

const (
	signupPerIP    = 10  // an hour; carrier NAT puts many phones behind one IP
	signupPerAgent = 300 // an hour; a backstop if the IP signal is spoofed
)

// clientIP prefers the left-most X-Forwarded-For entry, which the platform in
// front of this service (Vercel, then Render) sets to the caller. A caller
// hitting this service directly could forge it, which only lets them dodge the
// per-IP limit — the per-Agent limit still holds.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if ip := net.ParseIP(first); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Signup creates a Player under the Agent identified by the invite code, then
// enrolls this phone and signs it in, in one transaction: a failure anywhere
// leaves no half-made account behind.
func Signup(pool *pgxpool.Pool) http.HandlerFunc {
	perIP := newThrottle(time.Hour)
	perAgent := newThrottle(time.Hour)

	return func(w http.ResponseWriter, r *http.Request) {
		var body signupRequestBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			http.Error(w, "bad_request", http.StatusBadRequest)
			return
		}

		name := strings.TrimSpace(spaces.ReplaceAllString(body.Name, " "))
		if !nameRe.MatchString(name) {
			http.Error(w, "bad_name", http.StatusBadRequest)
			return
		}
		phone, ok := normalizePhone(body.Phone)
		if !ok {
			http.Error(w, "bad_phone", http.StatusBadRequest)
			return
		}
		if !pinRe.MatchString(body.Pin) {
			http.Error(w, "bad_pin", http.StatusBadRequest)
			return
		}
		if len(body.AgentCode) < 4 || len(body.AgentCode) > 32 {
			http.Error(w, "agent_not_found", http.StatusNotFound)
			return
		}

		// Counted before any database work, and counting failures too: a
		// script guessing invite codes burns its allowance like any other.
		if !perIP.allow(clientIP(r), signupPerIP) || !perAgent.allow(body.AgentCode, signupPerAgent) {
			http.Error(w, "rate_limited", http.StatusTooManyRequests)
			return
		}

		var agentID string
		var adminID *string
		err := pool.QueryRow(r.Context(), `
			SELECT id, created_by_id FROM users
			WHERE invite_code = $1 AND account_type = 'AGENT' AND is_active
		`, body.AgentCode).Scan(&agentID, &adminID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && adminID == nil) {
			http.Error(w, "agent_not_found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		// Never typed, never shown: it only exists because users.password_hash
		// is NOT NULL, and keeps the old password form from ever matching.
		// Cost 4 because a random 48-hex-char secret gains nothing from more.
		secret := make([]byte, 24)
		if _, err := rand.Read(secret); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		passwordHash, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(secret)), bcrypt.MinCost)
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		pinHash, err := bcrypt.GenerateFromPassword([]byte(body.Pin), 10)
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		deviceKey, err := generateSessionToken()
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		tx, err := pool.Begin(r.Context())
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(r.Context())

		// Same rule core-service applies when an Agent creates a Player: a
		// phone may play once per Admin (two Admins are independent
		// businesses). No DB constraint backs it, so serialize on
		// (admin, phone) — otherwise two simultaneous sign-ups both pass the
		// check below.
		if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext($1))`, *adminID+":"+phone); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		var taken bool
		if err := tx.QueryRow(r.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM users p
				JOIN users a ON a.id = p.agent_id
				WHERE p.account_type = 'PLAYER' AND p.email = $1 AND a.created_by_id = $2
			)
		`, phone, *adminID).Scan(&taken); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		if taken {
			http.Error(w, "phone_taken", http.StatusConflict)
			return
		}

		username := usernameBase(name, phone)
		for attempt := 0; ; attempt++ {
			var clash bool
			if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, username).Scan(&clash); err != nil {
				http.Error(w, "server_error", http.StatusInternalServerError)
				return
			}
			if !clash {
				break
			}
			if attempt >= 5 {
				http.Error(w, "try_again", http.StatusConflict)
				return
			}
			n, _ := rand.Int(rand.Reader, big.NewInt(90))
			username = fmt.Sprintf("%s%d", usernameBase(name, phone), 10+n.Int64())
		}

		userID := db.NewUUID()
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO users (id, email, username, password_hash, account_type, created_by_id, agent_id, updated_at)
			VALUES ($1, $2, $3, $4, 'PLAYER', $5, $5, now())
		`, userID, phone, username, string(passwordHash), agentID); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		// Filed under the Agent whose app this was, with the player as the actor
		// (nobody on staff did this).
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO audit_logs (id, owner_id, actor_id, actor_type, action, target_user_id, meta)
			VALUES ($1, $2, $3, 'PLAYER', 'player.signup', $3, $4::jsonb)
		`, db.NewUUID(), agentID, userID, fmt.Sprintf(`{"username":%q}`, username)); err != nil {
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
				ID: userID, Email: phone, Username: username, AccountType: "PLAYER",
				AgentID: &agentID, Permissions: []string{},
			},
			"deviceId":  deviceID,
			"deviceKey": deviceKey,
		})
	}
}
