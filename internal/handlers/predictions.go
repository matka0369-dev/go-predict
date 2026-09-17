package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
	"predictsim/prediction-service/internal/game"
	"predictsim/prediction-service/internal/prediction"
)

type placeRequestBody struct {
	GameID       string `json:"gameId"`
	TypeID       string `json:"typeId"`
	PickedNumber string `json:"pickedNumber"`
	Stake        int    `json:"stake"`
}

type placeResponseBody struct {
	PredictionID   string `json:"predictionId"`
	OddsMultiplier int    `json:"oddsMultiplier"`
	BalanceAfter   int    `json:"balanceAfter"`
}

// PostPrediction is the one write endpoint this service exposes. See
// prediction.Place for the actual logic — this handler is just request
// parsing, the account-type gate (only a native Player ever bets; staff and
// every tier above have no token authority to place bets with), and error
// translation to HTTP status codes.
func PostPrediction(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "Only Players may place predictions", http.StatusForbidden)
			return
		}

		var body placeRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if body.GameID == "" || body.Stake <= 0 {
			http.Error(w, "gameId and a positive stake are required", http.StatusBadRequest)
			return
		}

		result, err := prediction.Place(r.Context(), pool, user, prediction.PlaceRequest{
			GameID:       body.GameID,
			TypeID:       prediction.Type(body.TypeID),
			PickedNumber: body.PickedNumber,
			Stake:        body.Stake,
		})
		if err != nil {
			writeError(w, err)
			return
		}

		writeJSON(w, http.StatusCreated, placeResponseBody{
			PredictionID:   result.PredictionID,
			OddsMultiplier: result.OddsMultiplier,
			BalanceAfter:   result.BalanceAfter,
		})
	}
}

type predictionRoundRef struct {
	ID   string            `json:"id"`
	Date string            `json:"date"`
	Game predictionGameRef `json:"game"`
}

type predictionGameRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type predictionHistoryEntry struct {
	ID             string             `json:"id"`
	TypeID         string             `json:"typeId"`
	PickedNumber   string             `json:"pickedNumber"`
	Stake          int                `json:"stake"`
	OddsMultiplier int                `json:"oddsMultiplier"`
	Outcome        string             `json:"outcome"`
	Payout         *int               `json:"payout"`
	CreatedAt      string             `json:"createdAt"`
	User           predictionUserRef  `json:"user"`
	Round          predictionRoundRef `json:"round"`
}

type predictionUserRef struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// GetMyPredictions mirrors PredictionsService.myPredictions — a Player's
// own bet history, most recent first. No other tier's Prediction rows exist
// to leak: userId on this table only ever points at a native Player.
func GetMyPredictions(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "Only Players have prediction history", http.StatusForbidden)
			return
		}

		rows, err := pool.Query(r.Context(), `
			SELECT p.id, p.type_id, p.picked_number, p.stake, p.odds_multiplier, p.outcome, p.payout, p.created_at,
			       u.id, u.username,
			       rd.id, rd.date, g.id, g.name
			FROM predictions p
			JOIN users u ON u.id = p.user_id
			JOIN rounds rd ON rd.id = p.round_id
			JOIN games g ON g.id = rd.game_id
			WHERE p.user_id = $1
			ORDER BY p.created_at DESC
		`, user.ID)
		if err != nil {
			http.Error(w, "Failed to load prediction history", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := []predictionHistoryEntry{}
		for rows.Next() {
			var (
				e                    predictionHistoryEntry
				createdAt, roundDate time.Time
			)
			if err := rows.Scan(
				&e.ID, &e.TypeID, &e.PickedNumber, &e.Stake, &e.OddsMultiplier, &e.Outcome, &e.Payout, &createdAt,
				&e.User.ID, &e.User.Username,
				&e.Round.ID, &roundDate, &e.Round.Game.ID, &e.Round.Game.Name,
			); err != nil {
				http.Error(w, "Failed to load prediction history", http.StatusInternalServerError)
				return
			}
			e.CreatedAt = createdAt.Format(time.RFC3339)
			e.Round.Date = roundDate.Format("2006-01-02")
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "Failed to load prediction history", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, out)
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, game.ErrNotFound), errors.Is(err, prediction.ErrGameNotFound):
		status = http.StatusNotFound
	case errors.Is(err, prediction.ErrGameDisabled),
		errors.Is(err, prediction.ErrGameNotActive),
		errors.Is(err, prediction.ErrRoundCancelled),
		errors.Is(err, prediction.ErrPastCutoff),
		errors.Is(err, prediction.ErrAlreadySettled),
		errors.Is(err, prediction.ErrInsufficientBalance):
		status = http.StatusForbidden
	// A stake the payout column cannot represent is a bad request, not a
	// closed window — the Player can retry with a smaller one.
	case errors.Is(err, prediction.ErrPayoutTooLarge):
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}
