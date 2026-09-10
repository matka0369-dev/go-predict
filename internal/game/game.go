// Package game resolves "what can a Player bet on right now" and "is this
// specific game/round open for this typeId right now" — the read side used
// by both the games/active listing and the placement path's pre-checks.
// Actual cutoff *enforcement* lives in the DB trigger (see the
// redesign_prediction_games migration); the checks here are a friendlier,
// faster-failing mirror of the same rule, not the authority on it.
package game

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// ResolveAdminID walks Agent -> its creating Admin, the same "whose subtree
// is this" resolution core-service's resolveScopeOwnerId performs — a
// native Agent's createdById always is its Admin.
func ResolveAdminID(ctx context.Context, pool *pgxpool.Pool, agentID string) (string, error) {
	var adminID string
	err := pool.QueryRow(ctx, `SELECT created_by_id FROM users WHERE id = $1`, agentID).Scan(&adminID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return adminID, err
}

type ActiveGame struct {
	GameID      string
	Name        string
	Description *string
	MinStake    int
	MaxStake    int
	RoundID     string
	Date        time.Time
	OpensAt     time.Time
	ClosesAt    time.Time
	RoundStatus string
}

// ActiveGamesForPlayer lists every game the Player's Admin has enabled that
// has a Round generated for today — what the Predict UI needs to render. A
// Game with no Round yet (scheduler hasn't caught up, or today is a leave
// day) simply doesn't appear; that's correct, not a bug to work around here.
//
// "Today" is resolved per game, in that game's own zone, inside the query.
// It cannot be a parameter: two games in different zones can be on
// different calendar days at the same instant, so one date computed by the
// caller would be wrong for at least one of them.
func ActiveGamesForPlayer(ctx context.Context, pool *pgxpool.Pool, agentID string) ([]ActiveGame, error) {
	adminID, err := ResolveAdminID(ctx, pool, agentID)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT g.id, g.name, g.description, g.min_stake, g.max_stake,
		       r.id, r.date, r.opens_at, r.closes_at, r.status
		FROM games g
		JOIN game_enablements ge ON ge.game_id = g.id AND ge.admin_id = $1 AND ge.enabled = true
		JOIN rounds r ON r.game_id = g.id
		                 AND r.date = (now() AT TIME ZONE g.timezone)::date
		WHERE g.status = 'ACTIVE'
		ORDER BY g.name
	`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ActiveGame
	for rows.Next() {
		var g ActiveGame
		if err := rows.Scan(&g.GameID, &g.Name, &g.Description, &g.MinStake, &g.MaxStake,
			&g.RoundID, &g.Date, &g.OpensAt, &g.ClosesAt, &g.RoundStatus); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type RoundLookup struct {
	RoundID     string
	OpensAt     time.Time
	ClosesAt    time.Time
	RoundStatus string
	GameStatus  string
	MinStake    int
	MaxStake    int
	Enabled     bool
	// Whichever panas have already been published for this round. A side
	// that has one is settled: accepting a bet against it would debit a
	// stake that can never be graded, because settlement only ever runs
	// from the result submission that is now refused as a duplicate.
	OpenPana  *string
	ClosePana *string
}

// TodaysRound resolves the round a placement request against gameId
// actually targets, plus everything the placement handler needs to
// pre-validate before attempting the debit — the game's own lifecycle
// status, whether the Player's Admin has it enabled, and the stake bounds.
// Not authoritative on the cutoff itself; see the package doc.
//
// The target day is the game's own, in the game's zone — see
// ActiveGamesForPlayer for why it isn't a caller-supplied parameter.
func TodaysRound(ctx context.Context, pool *pgxpool.Pool, gameID, adminID string) (*RoundLookup, error) {
	var r RoundLookup
	err := pool.QueryRow(ctx, `
		SELECT r.id, r.opens_at, r.closes_at, r.status, g.status, g.min_stake, g.max_stake,
		       EXISTS(
		         SELECT 1 FROM game_enablements ge
		         WHERE ge.game_id = g.id AND ge.admin_id = $2 AND ge.enabled = true
		       ) AS enabled,
		       r.open_pana, r.close_pana
		FROM rounds r
		JOIN games g ON g.id = r.game_id
		WHERE r.game_id = $1 AND r.date = (now() AT TIME ZONE g.timezone)::date
	`, gameID, adminID).Scan(
		&r.RoundID, &r.OpensAt, &r.ClosesAt, &r.RoundStatus, &r.GameStatus, &r.MinStake, &r.MaxStake, &r.Enabled,
		&r.OpenPana, &r.ClosePana,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
