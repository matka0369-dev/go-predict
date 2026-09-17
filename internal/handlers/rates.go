package handlers

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
)

// betTypes/betTypeLabel/profitShareTotal mirror core-service's
// rates.constants.ts exactly — static, no DB, same 7 bet types in the same
// order everywhere else in this codebase enumerates them.
var betTypes = []string{
	"SINGLE", "JODI", "SINGLE_PANA", "DOUBLE_PANA", "TRIPLE_PANA", "HALF_SANGAM", "FULL_SANGAM",
}

var betTypeLabel = map[string]string{
	"SINGLE":      "Single",
	"JODI":        "Jodi",
	"SINGLE_PANA": "Single Pana",
	"DOUBLE_PANA": "Double Pana",
	"TRIPLE_PANA": "Triple Pana",
	"HALF_SANGAM": "Half Sangam",
	"FULL_SANGAM": "Full Sangam",
}

const profitShareTotal = 10

type betTypeMeta struct {
	BetType string `json:"betType"`
	Label   string `json:"label"`
}

// GetRateMeta mirrors RatesController's `GET /rates/meta` — pure constants,
// no DB access, so it needs no account-type check.
func GetRateMeta(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := make([]betTypeMeta, 0, len(betTypes))
		for _, bt := range betTypes {
			out = append(out, betTypeMeta{BetType: bt, Label: betTypeLabel[bt]})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"betTypes":         out,
			"profitShareTotal": profitShareTotal,
		})
	}
}

type rateEntry struct {
	BetType    string `json:"betType"`
	Multiplier int    `json:"multiplier"`
	UpdatedAt  string `json:"updatedAt"`
}

// GetMyRates mirrors RatesService.myCards's PLAYER branch only — this
// service's only caller is the Player portal, so the ADMIN/AGENT branches
// (still Node-only) don't need a Go port. A Player with no agent (shouldn't
// happen once a request reaches here, since a native Player always has one,
// but mirrors the Node guard) gets an empty card rather than a 500.
func GetMyRates(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "This account tier has no rate card", http.StatusForbidden)
			return
		}
		if user.AgentID == nil {
			writeJSON(w, http.StatusOK, map[string]any{"kind": "PLAYER", "playing": []rateEntry{}})
			return
		}

		own, err := rateCard(r, pool, user.ID, "PLAYING")
		if err != nil {
			http.Error(w, "Failed to load rates", http.StatusInternalServerError)
			return
		}
		if len(own) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"kind": "PLAYER", "playing": own})
			return
		}

		giving, err := rateCard(r, pool, *user.AgentID, "GIVING")
		if err != nil {
			http.Error(w, "Failed to load rates", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "PLAYER", "playing": giving})
	}
}

func rateCard(r *http.Request, pool *pgxpool.Pool, ownerID, kind string) ([]rateEntry, error) {
	rows, err := pool.Query(r.Context(), `
		SELECT bet_type, multiplier, updated_at
		FROM rates
		WHERE owner_id = $1 AND kind = $2
		ORDER BY bet_type ASC
	`, ownerID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []rateEntry{}
	for rows.Next() {
		var (
			betType    string
			multiplier int
			updatedAt  time.Time
		)
		if err := rows.Scan(&betType, &multiplier, &updatedAt); err != nil {
			return nil, err
		}
		out = append(out, rateEntry{BetType: betType, Multiplier: multiplier, UpdatedAt: updatedAt.Format(time.RFC3339)})
	}
	return out, rows.Err()
}
