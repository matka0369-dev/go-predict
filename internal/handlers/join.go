package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GetJoin is public: it tells the player app which WhatsApp number an
// Agent's invite code points at, so the Register button can open a chat with
// the right person. The only things it returns are the Agent's display name
// and the number the Agent chose to publish for this purpose.
//
// An unknown code, an inactive Agent and an Agent with no number all look the
// same (404): there's nothing a caller can learn about which codes exist.
func GetJoin(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		if len(code) < 4 || len(code) > 32 {
			http.Error(w, "not_found", http.StatusNotFound)
			return
		}

		var name, number string
		err := pool.QueryRow(r.Context(), `
			SELECT username, whatsapp_number
			FROM users
			WHERE invite_code = $1
			  AND account_type = 'AGENT'
			  AND is_active
			  AND whatsapp_number IS NOT NULL
		`, code).Scan(&name, &number)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not_found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"agentName": name, "whatsappNumber": number})
	}
}
