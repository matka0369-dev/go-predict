// Package db wires the pgx connection pool. Everything else in this service
// takes a *pgxpool.Pool directly and writes its own SQL — no ORM, since this
// service only ever needs a handful of hand-tuned queries against tables
// core-service's Prisma schema already owns (see DATABASE.md).
package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, databaseURL)
}
