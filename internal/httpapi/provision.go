package httpapi

import (
	"context"
	"log"
	"time"

	"github.com/Venapce/venapce-api/internal/superset"
)

// supersetDataTables are the Venapce data tables that should show up in Superset
// as datasets, ready to chart against. These are the two data tables (stage and
// issues); the settings/charts/dashboards tables are backend metadata only.
var supersetDataTables = []string{"stage", "issues"}

// StartSupersetProvisioning kicks off, in the background, the one-time wiring of
// Superset: register the venapce Postgres as a database connection and add the
// data tables (stage, issues) as datasets. It never blocks boot — the app listens
// immediately and this reports its progress through the logs. If Superset is not
// reachable yet (still starting, or not configured), it retries for a while and
// then gives up quietly.
func (s *Server) StartSupersetProvisioning() {
	go s.provisionSuperset()
}

func (s *Server) provisionSuperset() {
	const (
		attempts = 20
		delay    = 15 * time.Second
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		client := s.sup.Get()
		if client == nil {
			// Not configured yet — nothing to do. Saving Superset settings will
			// trigger provisioning again, so we don't need to keep spinning here.
			log.Printf("superset provisioning: skipped (Superset not configured)")
			return
		}
		if err := s.runSupersetProvisioning(context.Background(), client); err != nil {
			log.Printf("superset provisioning: attempt %d/%d failed: %v", attempt, attempts, err)
			time.Sleep(delay)
			continue
		}
		log.Printf("superset provisioning: done")
		return
	}
	log.Printf("superset provisioning: gave up after %d attempts (Superset unreachable) — configure it and re-save settings to retry", attempts)
}

// runSupersetProvisioning performs one full pass: ensure the database connection,
// then ensure each data table's dataset. Any failure aborts the pass and is
// returned so the caller can retry.
func (s *Server) runSupersetProvisioning(ctx context.Context, client *superset.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	dbID, created, err := client.EnsureDatabase(ctx, superset.DBConnection{
		Name:     s.cfg.SupersetDBName,
		Host:     s.cfg.SupersetDB.Host,
		Port:     s.cfg.SupersetDB.Port,
		Database: s.cfg.SupersetDB.Database,
		Username: s.cfg.SupersetDB.Username,
		Password: s.cfg.SupersetDB.Password,
		SSLMode:  s.cfg.SupersetDB.SSLMode,
	})
	if err != nil {
		return err
	}
	if created {
		log.Printf("superset provisioning: created database connection %q (id=%d)", s.cfg.SupersetDBName, dbID)
	} else {
		log.Printf("superset provisioning: database connection %q already present (id=%d)", s.cfg.SupersetDBName, dbID)
	}

	for _, table := range supersetDataTables {
		id, created, err := client.EnsureDataset(ctx, dbID, s.cfg.SupersetSchema, table)
		if err != nil {
			return err
		}
		if created {
			log.Printf("superset provisioning: added dataset %q (id=%d)", table, id)
		} else {
			log.Printf("superset provisioning: dataset %q already present (id=%d)", table, id)
		}
	}
	return nil
}
