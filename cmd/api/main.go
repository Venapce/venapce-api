package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Venapce/venapce-api/internal/config"
	"github.com/Venapce/venapce-api/internal/cryptobox"
	"github.com/Venapce/venapce-api/internal/httpapi"
	"github.com/Venapce/venapce-api/internal/store"
	"github.com/Venapce/venapce-api/internal/version"
)

func main() {
	// First line in the log, before anything can fail: which build is running.
	log.Printf("venapce-api version %s", version.Current())

	cfg := config.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping postgres: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	box, err := cryptobox.New(cfg.AppSecret)
	if err != nil {
		log.Fatalf("cryptobox: %v", err)
	}

	srv := httpapi.New(pool, box, cfg)
	if err := srv.LoadSupersetFromDB(ctx); err != nil {
		log.Printf("warning: could not load stored Superset settings: %v", err)
	}
	if err := srv.LoadOsctrlFromDB(ctx); err != nil {
		log.Printf("warning: could not load stored osctrl settings: %v", err)
	}
	// Where FloMorphic is: env (FLOMORPHIC_URL / FLOMORPHIC_JWT_SECRET / INFRA_HOST)
	// is the default, and anything the operator saved in Settings overrides it. Done
	// before the plugin starts so a corrected address is in force from the first call.
	if err := srv.LoadFlomorphicFromDB(ctx); err != nil {
		log.Printf("warning: could not load stored FloMorphic settings: %v", err)
	}

	// Connect the in-process venapce plugin (db.* + osquery.* actions) to infra
	// using the stored FloMorphic plugin env. A no-op until the operator has
	// pasted that env; never blocks boot.
	if err := srv.StartVenapcePlugin(ctx); err != nil {
		log.Printf("warning: could not start venapce plugin: %v", err)
	}

	// In the packaged product Superset is bootstrapped by the deploy compose;
	// self-configure the connection from env so the operator never types it. A
	// no-op in dev without the env, or when the operator uses an external Superset.
	if err := srv.BootstrapSupersetFromEnv(ctx); err != nil {
		log.Printf("warning: could not bootstrap Superset from env: %v", err)
	}

	// Lazily wire Superset (register the venapce Postgres + data-table datasets)
	// in the background so it never delays the listener; progress goes to the log.
	srv.StartSupersetProvisioning()

	app := srv.App()
	log.Printf("venapce-api %s listening on :%s", version.Current(), cfg.Port)
	if err := app.Listen(":" + cfg.Port); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
