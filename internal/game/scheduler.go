package game

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/db"
)

type activeGameDef struct {
	ID            string
	OpenHour      int
	OpenMinute    int
	CloseHour     int
	CloseMinute   int
	WeeklyOffDays []int32
	Timezone      string
}

// GenerateRounds ensures every ACTIVE game has a Round row for each of the
// next daysAhead days (today included), skipping weeklyOffDays and
// GameHoliday dates — the "leave days" Platform Admin configures. Safe to
// call repeatedly and concurrently: the (game_id, date) unique constraint
// plus ON CONFLICT DO NOTHING make this idempotent rather than something
// that has to run at one precise moment.
func GenerateRounds(ctx context.Context, pool *pgxpool.Pool, daysAhead int) error {
	rows, err := pool.Query(ctx, `
		SELECT id,
		       EXTRACT(HOUR FROM open_time)::int, EXTRACT(MINUTE FROM open_time)::int,
		       EXTRACT(HOUR FROM close_time)::int, EXTRACT(MINUTE FROM close_time)::int,
		       weekly_off_days, timezone
		FROM games WHERE status = 'ACTIVE'
	`)
	if err != nil {
		return err
	}
	var games []activeGameDef
	for rows.Next() {
		var g activeGameDef
		if err := rows.Scan(&g.ID, &g.OpenHour, &g.OpenMinute, &g.CloseHour, &g.CloseMinute,
			&g.WeeklyOffDays, &g.Timezone); err != nil {
			rows.Close()
			return err
		}
		games = append(games, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, g := range games {
		// A game whose zone the platform can't resolve is skipped rather
		// than generated in the wrong one: a Round's instants are fixed at
		// generation and never rewritten, so guessing here would bake the
		// error into every day going forward.
		loc, err := time.LoadLocation(g.Timezone)
		if err != nil {
			log.Printf("game %s: unknown timezone %q, skipping round generation: %v", g.ID, g.Timezone, err)
			continue
		}

		// "Today" is the game's own calendar day, not the UTC one — for
		// Asia/Kolkata those differ for five and a half hours of every day.
		nowLocal := time.Now().In(loc)
		offDays := map[int]bool{}
		for _, d := range g.WeeklyOffDays {
			offDays[int(d)] = true
		}

		for offset := range daysAhead {
			local := nowLocal.AddDate(0, 0, offset)
			// Midnight local, so `date` is the calendar day a player and an
			// operator would both name.
			date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

			if offDays[int(date.Weekday())] {
				continue
			}

			// The date is formatted rather than passed as a time.Time:
			// local midnight is an instant on the previous UTC day for any
			// positive offset, and letting the driver convert it would
			// compare against the wrong calendar date.
			var isHoliday bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM game_holidays WHERE game_id = $1 AND date = $2)
			`, g.ID, date.Format("2006-01-02")).Scan(&isHoliday); err != nil {
				return err
			}
			if isHoliday {
				continue
			}

			// Built with time.Date in the zone rather than by adding an
			// offset to midnight: on a DST boundary the wall-clock time is
			// what the game means, and midnight+Nh is not the same instant.
			// Stored in UTC, matching the naive-UTC convention of the
			// timestamp columns.
			opensAt := time.Date(date.Year(), date.Month(), date.Day(), g.OpenHour, g.OpenMinute, 0, 0, loc).UTC()
			closesAt := time.Date(date.Year(), date.Month(), date.Day(), g.CloseHour, g.CloseMinute, 0, 0, loc).UTC()

			if _, err := pool.Exec(ctx, `
				INSERT INTO rounds (id, game_id, date, opens_at, closes_at, status, updated_at)
				VALUES ($1, $2, $3, $4, $5, 'OPEN', now())
				ON CONFLICT (game_id, date) DO NOTHING
			`, db.NewUUID(), g.ID, date.Format("2006-01-02"), opensAt, closesAt); err != nil {
				return err
			}
		}
	}

	return nil
}

// RunScheduler generates rounds immediately, then again on every tick, so a
// freshly enabled game becomes bettable without waiting for the first
// interval to elapse. Logs and continues on error rather than crashing the
// service over a transient DB hiccup — the next tick tries again.
func RunScheduler(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, daysAhead int) {
	run := func() {
		if err := GenerateRounds(ctx, pool, daysAhead); err != nil {
			log.Printf("round generation failed: %v", err)
		}
	}

	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
