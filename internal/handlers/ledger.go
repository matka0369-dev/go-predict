package handlers

import (
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
)

var ledgerDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type ledgerUserRef struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	AccountType *string `json:"accountType,omitempty"`
}

type ledgerEntry struct {
	ID           string         `json:"id"`
	Delta        int            `json:"delta"`
	Wallet       string         `json:"wallet"`
	BalanceAfter int            `json:"balanceAfter"`
	Source       string         `json:"source"`
	Note         *string        `json:"note"`
	CreatedAt    string         `json:"createdAt"`
	User         ledgerUserRef  `json:"user"`
	PerformedBy  *ledgerUserRef `json:"performedBy"`
}

// GetLedger mirrors LedgerService.history's PLAYER branch only (`where
// userId = requester.id`) — the ADMIN/AGENT branches, including the
// `agentId` filter, are reviewer-side and stay Node-only. Same `seq DESC`
// ordering (write order, not clock order — see ledger.service.ts) and the
// same 500-row cap.
func GetLedger(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "This account tier has no player ledger", http.StatusForbidden)
			return
		}

		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 500 {
			limit = 500
		}

		var date *string
		if v := r.URL.Query().Get("date"); v != "" {
			if !ledgerDateRe.MatchString(v) {
				http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
				return
			}
			date = &v
		}

		query := `
			SELECT e.id, e.delta, e.wallet, e.balance_after, e.source, e.note, e.created_at,
			       u.id, u.username, u.account_type,
			       pb.id, pb.username
			FROM token_ledger_entries e
			JOIN users u ON u.id = e.user_id
			LEFT JOIN users pb ON pb.id = e.performed_by_id
			WHERE e.user_id = $1
		`
		args := []any{user.ID}
		if date != nil {
			query += ` AND e.created_at >= $2::date AND e.created_at < ($2::date + interval '1 day')`
			args = append(args, *date)
		}
		query += ` ORDER BY e.seq DESC LIMIT ` + strconv.Itoa(limit)

		rows, err := pool.Query(r.Context(), query, args...)
		if err != nil {
			http.Error(w, "Failed to load ledger", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := []ledgerEntry{}
		for rows.Next() {
			var (
				e                              ledgerEntry
				createdAt                      time.Time
				accountType                    string
				performedByID, performedByUser *string
			)
			if err := rows.Scan(
				&e.ID, &e.Delta, &e.Wallet, &e.BalanceAfter, &e.Source, &e.Note, &createdAt,
				&e.User.ID, &e.User.Username, &accountType,
				&performedByID, &performedByUser,
			); err != nil {
				http.Error(w, "Failed to load ledger", http.StatusInternalServerError)
				return
			}
			e.CreatedAt = createdAt.Format(time.RFC3339)
			e.User.AccountType = &accountType
			if performedByID != nil {
				e.PerformedBy = &ledgerUserRef{ID: *performedByID, Username: *performedByUser}
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "Failed to load ledger", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, out)
	}
}
