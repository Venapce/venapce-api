package store

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Migrate applies the schema. Every statement is IF NOT EXISTS, so running it on
// each boot is idempotent — enough for this stage; swap in versioned migrations
// (golang-migrate) once the schema starts changing in incompatible ways.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schema)
	return err
}
