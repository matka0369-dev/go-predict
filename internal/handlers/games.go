package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
	"predictsim/prediction-service/internal/game"
	"predictsim/prediction-service/internal/prediction"
)

type activeGameResponse struct {
	GameID      string           `json:"gameId"`
	Name        string           `json:"name"`
	Description *string          `json:"description"`
	MinStake    int              `json:"minStake"`
	MaxStake    int              `json:"maxStake"`
	RoundID     string           `json:"roundId"`
	Date        string           `json:"date"`
	OpensAt     time.Time        `json:"opensAt"`
	ClosesAt    time.Time        `json:"closesAt"`
	Cutoffs     map[string]string `json:"cutoffs"`
}

// GetActiveGames serves the Predict UI's game list: every game the
// Player's Admin has enabled, with today's round and a ready-to-render
// cutoff timestamp per prediction type — the UI is the third layer of the
// triple-layer cutoff check, and this is the data it disables controls off
// of.
func GetActiveGames(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "Only Players have games to bet on", http.StatusForbidden)
			return
		}
		if user.AgentID == nil {
			writeJSON(w, http.StatusOK, []activeGameResponse{})
			return
		}

		games, err := game.ActiveGamesForPlayer(r.Context(), pool, *user.AgentID)
		if err != nil {
			http.Error(w, "Failed to load games", http.StatusInternalServerError)
			return
		}

		out := make([]activeGameResponse, 0, len(games))
		for _, g := range games {
			cutoffs := make(map[string]string, len(prediction.AllTypes))
			for _, t := range prediction.AllTypes {
				cutoff := g.OpensAt.Add(-time.Minute)
				if t.CutoffGroup() == "close" {
					cutoff = g.ClosesAt.Add(-time.Minute)
				}
				cutoffs[string(t)] = cutoff.Format(time.RFC3339)
			}

			out = append(out, activeGameResponse{
				GameID:      g.GameID,
				Name:        g.Name,
				Description: g.Description,
				MinStake:    g.MinStake,
				MaxStake:    g.MaxStake,
				RoundID:     g.RoundID,
				Date:        g.Date.Format("2006-01-02"),
				OpensAt:     g.OpensAt,
				ClosesAt:    g.ClosesAt,
				Cutoffs:     cutoffs,
			})
		}

		writeJSON(w, http.StatusOK, out)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
