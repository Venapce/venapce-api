// Package httpapi wires the Fiber HTTP server: Superset connection settings,
// the server-side Superset data proxy, and CRUD for native Venapce charts and
// dashboards.
package httpapi

import (
	"context"
	"encoding/json"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/inflowenger/venapce-api/internal/config"
	"github.com/inflowenger/venapce-api/internal/cryptobox"
	"github.com/inflowenger/venapce-api/internal/db"
	"github.com/inflowenger/venapce-api/internal/superset"
)

type Server struct {
	q   *db.Queries
	box *cryptobox.Box
	sup *superset.Manager
	cfg config.Config
}

func New(pool *pgxpool.Pool, box *cryptobox.Box, cfg config.Config) *Server {
	return &Server{
		q:   db.New(pool),
		box: box,
		sup: superset.NewManager(),
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
		AllowOrigins: s.cfg.CORSOrigins,
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
		AllowMethods: "GET, POST, PUT, DELETE, OPTIONS",
	}))

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	api := app.Group("/api")

	// Superset connection settings (moved off the browser login screen).
	api.Get("/settings/superset", s.getSupersetSettings)
	api.Put("/settings/superset", s.putSupersetSettings)
	api.Post("/settings/superset/test", s.testSupersetSettings)

	// Server-side Superset data proxy (token stays here, never in the browser).
	sup := api.Group("/superset")
	sup.Get("/databases", s.supersetDatabases)
	sup.Get("/datasets", s.supersetDatasets)
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

func errorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}

// rawOr returns m when it is non-empty JSON, else the given fallback literal.
func rawOr(m json.RawMessage, fallback string) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage(fallback)
	}
	return m
}
