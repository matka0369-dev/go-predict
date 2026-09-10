package prediction

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
	"predictsim/prediction-service/internal/db"
	"predictsim/prediction-service/internal/game"
	"predictsim/prediction-service/internal/rates"
)

type PlaceRequest struct {
	GameID       string
	TypeID       Type
	PickedNumber string
	Stake        int
}

type PlaceResult struct {
	PredictionID   string
	OddsMultiplier int
	// Combined spendable total after the stake came out — main + winnings.
	// A single number because that's what the Player is actually able to bet
	// with next; the split is visible on the dashboard.
	BalanceAfter int
}

var (
	ErrInvalidType         = errors.New("invalid prediction type")
	ErrGameNotFound        = errors.New("game not found or not open today")
	ErrGameNotActive       = errors.New("game is not active")
	ErrGameDisabled        = errors.New("this game is not enabled for your account")
	ErrRoundCancelled      = errors.New("today's round has been cancelled")
	ErrPastCutoff          = errors.New("the betting window for this has closed")
	ErrStakeOutOfRange     = errors.New("stake is outside this game's allowed range")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrNoAgent             = errors.New("account has no agent")
	ErrAlreadySettled      = errors.New("the result for this round is already published")
	ErrPayoutTooLarge      = errors.New("stake is too large for this bet type's payout")
)

// maxPayout is the ceiling a settled payout may reach: predictions.payout,
// settlements.total_payout and settlement_lines.payout are all int4, and a
// payout past this overflows the column *inside* the settlement
// transaction. That rolls the whole submission back, which strands every
// bet on the round — not just the oversized one — because a result cannot
// be submitted twice. Rejecting the bet up front is the only point where
// the failure is still recoverable.
const maxPayout = math.MaxInt32

// Place validates and executes a bet, or fails without moving any tokens.
// The Go-layer checks here (game enabled, cutoff, stake bounds, pick
// format) are the fast, friendly path — they exist so an ordinary request
// gets a clear error instead of a raw Postgres exception, not because the
// DB-layer trigger/CHECK constraint added in the redesign_prediction_games
// migration are trusted to be redundant. That trigger runs inside the same
// transaction as the debit below and is the actual authority: if it
// disagrees with what this function believed, the whole transaction rolls
// back, so a rejected bet never debits regardless of which layer caught it.
func Place(ctx context.Context, pool *pgxpool.Pool, player auth.User, req PlaceRequest) (*PlaceResult, error) {
	if !IsValid(req.TypeID) {
		return nil, ErrInvalidType
	}
	if err := ValidatePickedNumber(req.TypeID, req.PickedNumber); err != nil {
		return nil, err
	}
	if player.AgentID == nil {
		return nil, ErrNoAgent
	}

	adminID, err := game.ResolveAdminID(ctx, pool, *player.AgentID)
	if err != nil {
		return nil, err
	}

	round, err := game.TodaysRound(ctx, pool, req.GameID, adminID)
	if errors.Is(err, game.ErrNotFound) {
		return nil, ErrGameNotFound
	}
	if err != nil {
		return nil, err
	}
	if round.GameStatus != "ACTIVE" {
		return nil, ErrGameNotActive
	}
	if !round.Enabled {
		return nil, ErrGameDisabled
	}
	if round.RoundStatus == "CANCELLED" {
		return nil, ErrRoundCancelled
	}
	if req.Stake < round.MinStake || req.Stake > round.MaxStake {
		return nil, fmt.Errorf("%w: must be between %d and %d", ErrStakeOutOfRange, round.MinStake, round.MaxStake)
	}

	cutoff := round.OpensAt.Add(-time.Minute)
	settledPana := round.OpenPana
	if req.TypeID.CutoffGroup() == "close" {
		cutoff = round.ClosesAt.Add(-time.Minute)
		settledPana = round.ClosePana
	}
	// Checked before the clock, because it is the stronger statement: a
	// published result means this side is graded and closed regardless of
	// what opens_at/closes_at say. Without it a result entered early (or
	// for a future date) leaves the window nominally open, and every bet
	// that arrives afterward is debited and then never graded.
	if settledPana != nil {
		return nil, ErrAlreadySettled
	}
	if time.Now().UTC().After(cutoff) {
		return nil, ErrPastCutoff
	}

	family, _ := req.TypeID.OddsFamily()
	odds, err := rates.EffectiveMultiplier(ctx, pool, player.ID, *player.AgentID, family)
	if errors.Is(err, rates.ErrNoRate) {
		return nil, fmt.Errorf("no rate configured for %s", family)
	}
	if err != nil {
		return nil, err
	}

	// int64 on purpose: the product is exactly what would overflow the int4
	// payout column, so it must not be computed in the type it would
	// overflow. See maxPayout for why this is fatal rather than clamped.
	if int64(req.Stake)*int64(odds) > maxPayout {
		return nil, fmt.Errorf("%w: %d at %dx would pay %d, above the %d limit",
			ErrPayoutTooLarge, req.Stake, odds, int64(req.Stake)*int64(odds), int64(maxPayout))
	}

	// The admin->agent rate for the same family, recorded but not applied —
	// see rates.AgentGivenMultiplier. Never fatal: a missing admin-side rate
	// must not stop a Player betting at a rate that does exist.
	agentOdds, err := rates.AgentGivenMultiplier(ctx, pool, *player.AgentID, family)
	if err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	// Spend the main wallet first and fall back to winnings for whatever it
	// couldn't cover. Conditional updates double as the row lock and the
	// overdraft check — same pattern as core-service's
	// LedgerService.applyDelta: if a balance moved underneath us, or would go
	// negative, zero rows match and the transaction fails rather than writing
	// a ledger entry that lies.
	//
	// Both legs are written as separate ledger rows when the stake straddles
	// the two wallets, because `balance_after` is per-wallet — one row
	// claiming to have moved both would have no honest value to record.
	fromMain, fromWinnings, mainAfter, winningsAfter, err := debitAcrossWallets(ctx, tx, player.ID, req.Stake)
	if err != nil {
		return nil, err
	}

	predictionID := db.NewUUID()
	_, err = tx.Exec(ctx, `
		INSERT INTO predictions (id, user_id, round_id, type_id, picked_number, stake, odds_multiplier, agent_odds_multiplier, cutoff_at, outcome, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'PENDING', now())
	`, predictionID, player.ID, round.RoundID, string(req.TypeID), req.PickedNumber, req.Stake, odds, agentOdds, cutoff)
	if err != nil {
		return nil, translatePredictionInsertError(err)
	}

	if fromMain > 0 {
		if err := insertLedger(ctx, tx, player.ID, -fromMain, "MAIN", mainAfter, "PREDICTION_DEBIT", &predictionID); err != nil {
			return nil, err
		}
	}
	if fromWinnings > 0 {
		if err := insertLedger(ctx, tx, player.ID, -fromWinnings, "WINNINGS", winningsAfter, "PREDICTION_DEBIT", &predictionID); err != nil {
			return nil, err
		}
	}

	// The stake has no counterparty on purpose (revision 2026-08-05, explicit
	// instruction): "the tokens from the player account gets disappears right
	// after he uses to predict and goes nowhere, just subtracting from his acc
	// creating a transaction."
	//
	// A previous iteration credited the Agent's wallet here
	// (PREDICTION_STAKE_IN). That's gone: wallets are now purely a
	// player-side concept, and the Admin<->Agent position is an accounting
	// record in `settlements` rather than tokens sloshing between wallets.
	// So total supply *does* shrink on a stake and grow on a payout, which is
	// intended and documented in ARCHITECTURE.md rather than a leak.

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &PlaceResult{
		PredictionID:   predictionID,
		OddsMultiplier: odds,
		BalanceAfter:   mainAfter + winningsAfter,
	}, nil
}

// debitAcrossWallets takes `stake` from the Player's main wallet first and
// the remainder from winnings, and reports how much came from each plus both
// resulting balances.
//
// The two-step is deliberate rather than a single combined check: each
// UPDATE's WHERE clause is its own overdraft guard against its own column,
// so a concurrent bet that drains one wallet between the two statements
// fails the second guard rather than overdrawing it. Both run inside the
// caller's transaction, so a failure anywhere rolls back the first leg too.
func debitAcrossWallets(
	ctx context.Context,
	tx pgx.Tx,
	playerID string,
	stake int,
) (fromMain, fromWinnings, mainAfter, winningsAfter int, err error) {
	var mainBalance, winningsBalance int
	if err = tx.QueryRow(ctx, `
		SELECT balance, winnings_balance FROM users WHERE id = $1 FOR UPDATE
	`, playerID).Scan(&mainBalance, &winningsBalance); err != nil {
		return 0, 0, 0, 0, err
	}

	if mainBalance+winningsBalance < stake {
		return 0, 0, 0, 0, ErrInsufficientBalance
	}

	fromMain = min(mainBalance, stake)
	fromWinnings = stake - fromMain
	mainAfter, winningsAfter = mainBalance, winningsBalance

	if fromMain > 0 {
		if err = tx.QueryRow(ctx, `
			UPDATE users SET balance = balance - $1
			WHERE id = $2 AND balance >= $1
			RETURNING balance
		`, fromMain, playerID).Scan(&mainAfter); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, 0, 0, 0, ErrInsufficientBalance
			}
			return 0, 0, 0, 0, err
		}
	}

	if fromWinnings > 0 {
		if err = tx.QueryRow(ctx, `
			UPDATE users SET winnings_balance = winnings_balance - $1
			WHERE id = $2 AND winnings_balance >= $1
			RETURNING winnings_balance
		`, fromWinnings, playerID).Scan(&winningsAfter); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, 0, 0, 0, ErrInsufficientBalance
			}
			return 0, 0, 0, 0, err
		}
	}

	return fromMain, fromWinnings, mainAfter, winningsAfter, nil
}

func insertLedger(
	ctx context.Context,
	tx pgx.Tx,
	userID string,
	delta int,
	wallet string,
	balanceAfter int,
	source string,
	predictionID *string,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO token_ledger_entries (id, user_id, delta, wallet, balance_after, source, prediction_id, created_at)
		VALUES ($1, $2, $3, $4::ledger_wallet, $5, $6::ledger_source, $7, now())
	`, db.NewUUID(), userID, delta, wallet, balanceAfter, source, predictionID)
	return err
}

// translatePredictionInsertError turns the DB-layer rejection (the
// picked_number CHECK constraint, or the enforce_prediction_cutoff
// trigger) into the same sentinel errors the Go-layer pre-checks use, so a
// caller doesn't need to know which of the three layers actually caught a
// given bad request.
func translatePredictionInsertError(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "23514": // check_violation
			return fmt.Errorf("%w: picked number failed validation", ErrInvalidType)
		case "P0001": // raise_exception, from enforce_prediction_cutoff
			return ErrPastCutoff
		}
	}
	return err
}
