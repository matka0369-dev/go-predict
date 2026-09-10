package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

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
