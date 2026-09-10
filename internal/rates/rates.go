// Package rates resolves what a Player is actually paid for a given bet
// type family — mirrors core-service's RatesService.myCards PLAYER branch
// exactly (see services/core-service/src/rates/rates.service.ts): a
// PLAYING override if the Player's Agent priced them differently at
// creation, else the Agent's current GIVING card, read live.
package rates

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNoRate = errors.New("no rate configured for this bet type")

// EffectiveMultiplier is the odds a Player would be given right now for
// betType, frozen into a Prediction at placement time so a later rate
// change never reprices a bet already placed.
func EffectiveMultiplier(ctx context.Context, pool *pgxpool.Pool, playerID, agentID, betType string) (int, error) {
	var multiplier int

	err := pool.QueryRow(ctx, `
		SELECT multiplier FROM rates WHERE owner_id = $1 AND kind = 'PLAYING' AND bet_type = $2
	`, playerID, betType).Scan(&multiplier)
	if err == nil {
		return multiplier, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	err = pool.QueryRow(ctx, `
		SELECT multiplier FROM rates WHERE owner_id = $1 AND kind = 'GIVING' AND bet_type = $2
	`, agentID, betType).Scan(&multiplier)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoRate
	}
	if err != nil {
		return 0, err
	}
	return multiplier, nil
}

// AgentGivenMultiplier is what the Admin pays the Agent at for betType —
// the Agent's own GIVEN card. Recorded on every Prediction alongside the
// player-side rate so the per-tier P&L split stays computable later; since
// the giving cap was removed (2026-08-05) the two can differ in either
// direction, and the gap is the Agent's own margin or its own liability.
//
// Returns 0 rather than an error when the Agent has no GIVEN row: a missing
// admin-side rate must never block a Player's bet, because the player-side
// rate is what actually prices it. 0 reads as "not recorded", consistent
// with the column default for predictions placed before this existed.
func AgentGivenMultiplier(ctx context.Context, pool *pgxpool.Pool, agentID, betType string) (int, error) {
	var multiplier int
	err := pool.QueryRow(ctx, `
		SELECT multiplier FROM rates WHERE owner_id = $1 AND kind = 'GIVEN' AND bet_type = $2
	`, agentID, betType).Scan(&multiplier)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return multiplier, nil
}
