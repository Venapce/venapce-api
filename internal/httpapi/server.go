// Package httpapi wires the Fiber HTTP server: Superset connection settings,
// the server-side Superset data proxy, and CRUD for native Venapce charts and
// dashboards.
package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Venapce/venapce-api/internal/config"
	"github.com/Venapce/venapce-api/internal/cryptobox"
	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/flomorphic"
	"github.com/Venapce/venapce-api/internal/osctrl"
	"github.com/Venapce/venapce-api/internal/plugin"
	"github.com/Venapce/venapce-api/internal/superset"
	"github.com/Venapce/venapce-api/internal/version"
)

type Server struct {
	q   *db.Queries
	box *cryptobox.Box
	sup *superset.Manager
	osc *osctrl.Manager
	plg *plugin.Manager
	// FloMorphic access (API base + shared secret + infra host), seeded from env
	// and replaceable at runtime from Settings. Holds the live client, which is nil
	// while that access is incomplete.
	flo *flomorphic.Manager
	cfg config.Config
}

func New(pool *pgxpool.Pool, box *cryptobox.Box, cfg config.Config) *Server {
	osc := osctrl.NewManager()
	return &Server{
		q:   db.New(pool),
		box: box,
		sup: superset.NewManager(),
		osc: osc,
		plg: plugin.NewManager(pool, osc),
		flo: flomorphic.NewManager(flomorphic.Access{
			URL:       cfg.FlomorphicURL,
			JWTSecret: cfg.FlomorphicJWTSecret,
			InfraHost: cfg.InfraHost,
		}),
		cfg: cfg,
	}
}

// App builds the Fiber app with all routes and middleware.
func (s *Server) App() *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "venapce-api",
		ErrorHandler: errorHandler,
	})
	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: splitCSV(s.cfg.CORSOrigins),
		AllowHeaders: []string{"Origin", "Content-Type", "Accept", "Authorization"},
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
	}))

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "version": version.Current()})
	})

	api := app.Group("/api")

	// The running build, so the panel can show the appliance version it is
	// talking to (its own is baked in at build time from the same VERSION).
	api.Get("/version", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"version": version.Current()})
	})

	// Superset connection settings (moved off the browser login screen).
	api.Get("/settings/superset", s.getSupersetSettings)
	api.Put("/settings/superset", s.putSupersetSettings)
	api.Post("/settings/superset/test", s.testSupersetSettings)
	// Revert an external-Superset override back to the built-in (env-managed) one.
	api.Post("/settings/superset/reset", s.resetSupersetSettings)
	// On-demand demo data: load Superset's example datasets from the UI.
	api.Get("/settings/superset/examples/status", s.getExamplesStatus)
	api.Post("/settings/superset/examples", s.loadExamples)

	// Server-side Superset data proxy (token stays here, never in the browser).
	sup := api.Group("/superset")
	sup.Get("/databases", s.supersetDatabases)
	sup.Get("/databases/:id/schemas", s.supersetDatabaseSchemas)
	sup.Get("/databases/:id/tables", s.supersetDatabaseTables)
	sup.Get("/datasets", s.supersetDatasets)
	sup.Post("/datasets", s.supersetCreateDataset)
	sup.Get("/datasets/:id", s.supersetDataset)
	sup.Get("/dashboards", s.supersetDashboards)
	sup.Post("/chart/data", s.supersetChartData)

	// Native Venapce charts.
	api.Get("/charts", s.listCharts)
	api.Post("/charts", s.createChart)
	api.Get("/charts/:id", s.getChart)
	api.Put("/charts/:id", s.updateChart)
	api.Delete("/charts/:id", s.deleteChart)

	// Native Venapce dashboards (title + grid layout).
	api.Get("/dashboards", s.listDashboards)
	api.Post("/dashboards", s.createDashboard)
	api.Get("/dashboards/:id", s.getDashboard)
	api.Put("/dashboards/:id", s.updateDashboard)
	api.Delete("/dashboards/:id", s.deleteDashboard)

	// osctrl connection settings + proxy (backs the Nodes area; JWT stays here).
	api.Get("/settings/osctrl", s.getOsctrlSettings)
	api.Put("/settings/osctrl", s.putOsctrlSettings)
	api.Post("/settings/osctrl/test", s.testOsctrlSettings)

	// FloMorphic plugin registration + the osctrl-space broker. Venapce is a
	// FloMorphic plugin; pasting the plugin env here lets the backend drive
	// infra's osspace flow and auto-wire a managed osctrl connection.
	api.Get("/settings/flomorphic", s.getFlomorphicSettings)
	api.Put("/settings/flomorphic", s.putFlomorphicSettings)
	// Where FloMorphic is: the env values (FLOMORPHIC_URL / FLOMORPHIC_JWT_SECRET /
	// INFRA_HOST) are only the default — these let an operator set them from the
	// panel, so an install that skipped FloMorphic can be wired up later without
	// editing .env and recreating the container.
	api.Put("/settings/flomorphic/api", s.putFlomorphicAPISettings)
	api.Post("/settings/flomorphic/api/test", s.testFlomorphicAPISettings)
	api.Post("/settings/flomorphic/api/reset", s.resetFlomorphicAPISettings)
	api.Post("/settings/flomorphic/osspace", s.connectOsspace)
	// (Re)start the in-process venapce plugin from the stored plugin env.
	api.Post("/settings/flomorphic/plugin/restart", s.restartVenapcePlugin)
	// Redefine venapce in FloMorphic: delete the extension row + re-register + re-sync.
	api.Post("/settings/flomorphic/plugin/refresh", s.refreshVenapcePlugin)
	// Report plugin connectivity as FloMorphic sees it (live @actions round-trip).
	api.Post("/settings/flomorphic/plugin/check", s.checkVenapcePlugin)
	osc := api.Group("/osctrl")
	osc.Get("/environments", s.osctrlEnvironments)
	osc.Get("/nodes", s.osctrlNodes)
	osc.Get("/nodes/:uuid", s.osctrlNode)
	osc.Get("/enroll", s.osctrlEnroll)
	osc.Post("/enroll/actions", s.osctrlEnrollAction)

	// Stage → Findings → Issues pipeline (rows produced/advanced by FloMorphic;
	// every level is optional — a flow may write to any of the three directly).
	api.Get("/stage", s.listStage)
	api.Post("/stage", s.createStage)
	api.Get("/stage/:id", s.getStage)
	api.Put("/stage/:id", s.updateStage)
	api.Delete("/stage/:id", s.deleteStage)
	api.Post("/stage/:id/promote", s.promoteStage) // ?to=finding|issue

	api.Get("/findings", s.listFindings)
	api.Get("/findings/tags", s.findingTags)
	api.Post("/findings", s.createFinding)
	api.Get("/findings/:id", s.getFinding)
	api.Put("/findings/:id", s.updateFinding)
	api.Delete("/findings/:id", s.deleteFinding)
	api.Post("/findings/:id/promote", s.promoteFinding)

	api.Get("/issues", s.listIssues)
	api.Get("/issues/tags", s.issueTags)
	api.Post("/issues", s.createIssue)
	api.Get("/issues/:id", s.getIssue)
	api.Put("/issues/:id", s.updateIssue)
	api.Delete("/issues/:id", s.deleteIssue)

	// Activities: the history of a pipeline row (edits, promotions) and the
	// outcome of every FloMorphic flow run on it. /flows lists what can be run.
	api.Get("/flows", s.listFlows)
	api.Get("/activities", s.listActivities)
	api.Get("/activities/tags", s.activityTags)
	api.Post("/activities", s.createActivity)
	api.Post("/activities/run", s.runFlow)
	api.Get("/activities/:id", s.getActivity)
	api.Put("/activities/:id", s.updateActivity)
	api.Delete("/activities/:id", s.deleteActivity)
	api.Post("/activities/:id/sync", s.syncActivityNow)
	api.Post("/activities/:id/stop", s.stopActivity)

	// Operations: installed packages of flows that originate pipeline data.
	// A package is stored whole; its flows are pushed into FloMorphic from here
	// (params substituted) and its entry flows run as activities on it.
	api.Get("/operations", s.listOperations)
	api.Post("/operations/import", s.importOperation) // {url|bundle, source, params, dryRun}
	api.Get("/operations/:id", s.getOperation)
	api.Delete("/operations/:id", s.deleteOperation)
	api.Post("/operations/:id/update", s.updateOperation)
	api.Put("/operations/:id/params", s.putOperationParams)
	api.Get("/operations/:id/flows/:key/file", s.getOperationFlowFile)
	api.Post("/operations/:id/flows/:key/install", s.installOperationFlow)
	api.Put("/operations/:id/flows/:key", s.linkOperationFlow)
	api.Post("/operations/:id/run", s.runOperation)

	return app
}

// LoadSupersetFromDB rebuilds the live Superset client from stored settings on
// boot. A fresh install with no settings yet simply leaves the manager empty.
func (s *Server) LoadSupersetFromDB(ctx context.Context) error {
	cfg, err := s.loadSupersetConfig(ctx)
	if err != nil || cfg == nil {
		return err
	}
	pass, err := s.box.Decrypt(cfg.PasswordEnc)
	if err != nil {
		return err
	}
	s.sup.Set(superset.NewClient(cfg.URL, cfg.Username, pass))
	return nil
}

// LoadOsctrlFromDB rebuilds the live osctrl client from stored settings on boot.
func (s *Server) LoadOsctrlFromDB(ctx context.Context) error {
	cfg, err := s.loadOsctrlConfig(ctx)
	if err != nil || cfg == nil {
		return err
	}
	pass, err := s.box.Decrypt(cfg.PasswordEnc)
	if err != nil {
		return err
	}
	s.osc.Set(osctrl.NewClient(cfg.URL, cfg.Username, pass, cfg.Environment))
	return nil
}

func errorHandler(c fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}

// splitCSV turns a comma-separated origins list into the []string that Fiber v3
// CORS expects, trimming surrounding spaces and dropping empty entries.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// rawOr returns m when it is non-empty JSON, else the given fallback literal.
func rawOr(m json.RawMessage, fallback string) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage(fallback)
	}
	return m
}
