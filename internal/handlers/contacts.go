package handlers

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	appauth "predictsim/prediction-service/internal/auth"
)

// GetMyContacts returns the WhatsApp numbers the signed-in Player's Agent has
// published, keyed by purpose ("support", "tokens", ...). Only the Agent's own
// numbers are reachable: the Agent comes from the Player's row, never from the
// request. A purpose the Agent hasn't set is simply absent; the app decides the
// fallback (tokens -> support).
func GetMyContacts(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := appauth.FromContext(r.Context())
		if user.AgentID == nil {
			writeJSON(w, http.StatusOK, map[string]any{"agentName": "", "contacts": map[string]string{}})
			return
		}

		rows, err := pool.Query(r.Context(), `
			SELECT u.username, c.purpose, c.whatsapp_number
			FROM users u
			LEFT JOIN agent_contacts c ON c.agent_id = u.id
			WHERE u.id = $1
		`, *user.AgentID)
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		agentName := ""
		contacts := map[string]string{}
		for rows.Next() {
			var name string
			var purpose, number *string
			if err := rows.Scan(&name, &purpose, &number); err != nil {
				http.Error(w, "server_error", http.StatusInternalServerError)
				return
			}
			agentName = name
			if purpose != nil && number != nil {
				contacts[*purpose] = *number
			}
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agentName": agentName, "contacts": contacts})
	}
}
