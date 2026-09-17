package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
)

type userRelationRef struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	AccountType *string `json:"accountType,omitempty"`
}

type userSummary struct {
	ID                string           `json:"id"`
	Email             string           `json:"email"`
	Username          string           `json:"username"`
	AccountType       string           `json:"accountType"`
	IsActive          bool             `json:"isActive"`
	AgentID           *string          `json:"agentId"`
	Balance           int              `json:"balance"`
	WinningsBalance   int              `json:"winningsBalance"`
	CreatedByID       *string          `json:"createdById"`
	CreatedAt         string           `json:"createdAt"`
	CreatedBy         *userRelationRef `json:"createdBy"`
	Agent             *userRelationRef `json:"agent"`
	AgentShare        *int             `json:"agentShare"`
	DefaultAgentShare *int             `json:"defaultAgentShare"`
}

// GetUser mirrors core-service's `GET /users/:id`, cut down to only what
// this service needs: a Player reading its own record for PredictForm's
// live balance display. Node's version is scoped by the caller's whole
// subtree (Admin/Agent looking up accounts they created); that authority
// model has no Go port and doesn't need one, since this service's only
// caller may only ever look up itself — self-scoped rather than open,
// enforced the same "404, not 403" way as everywhere else here.
func GetUser(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.FromContext(r.Context())
		id := chi.URLParam(r, "id")
		if id != caller.ID {
			http.Error(w, "User not found", http.StatusNotFound)
			return
		}

		var (
			u                              userSummary
			createdAt                      time.Time
			createdByID, createdByUsername *string
			createdByAccountType           *string
			agentUsername                  *string
		)
		err := pool.QueryRow(r.Context(), `
			SELECT u.id, u.email, u.username, u.account_type, u.is_active, u.agent_id,
			       u.balance, u.winnings_balance, u.created_by_id, u.created_at,
			       u.agent_share, u.default_agent_share,
			       cb.id, cb.username, cb.account_type,
			       ag.username
			FROM users u
			LEFT JOIN users cb ON cb.id = u.created_by_id
			LEFT JOIN users ag ON ag.id = u.agent_id
			WHERE u.id = $1
		`, id).Scan(
			&u.ID, &u.Email, &u.Username, &u.AccountType, &u.IsActive, &u.AgentID,
			&u.Balance, &u.WinningsBalance, &u.CreatedByID, &createdAt,
			&u.AgentShare, &u.DefaultAgentShare,
			&createdByID, &createdByUsername, &createdByAccountType,
			&agentUsername,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "User not found", http.StatusNotFound)
				return
			}
			http.Error(w, "Failed to load user", http.StatusInternalServerError)
			return
		}

		u.CreatedAt = createdAt.Format(time.RFC3339)
		if createdByID != nil {
			u.CreatedBy = &userRelationRef{ID: *createdByID, Username: *createdByUsername, AccountType: createdByAccountType}
		}
		if u.AgentID != nil {
			u.Agent = &userRelationRef{ID: *u.AgentID, Username: *agentUsername}
		}

		writeJSON(w, http.StatusOK, u)
	}
}
