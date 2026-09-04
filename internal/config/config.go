package config

import (
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config is the process configuration, all sourced from the environment so the
// same binary runs in the deploy compose (DATABASE_URL=...@postgres:5432/venapce)
// and locally.
type Config struct {
	Port        string
	DatabaseURL string
	AppSecret   string // key material for encrypting stored secrets (Superset password)
	CORSOrigins string // comma-separated allowed origins for the browser front

	// Superset provisioning: on boot we lazily register the venapce Postgres as a
	// Superset database connection and add the data tables (stage, issues) as
	// datasets, so charts can be built against them without manual setup. The
	// connection details are taken from DATABASE_URL — the very same Postgres the
	// API connects to — so there's nothing extra to configure.
	SupersetDBName string      // display name of the Superset database connection
	SupersetSchema string      // schema the data tables live in
	SupersetDB     PostgresDSN // discrete connection fields for Superset's add-database form
}

// PostgresDSN is a Postgres connection broken into discrete fields. We hand these
// to Superset's "add database" form individually (rather than as a URI string) so
// a password containing characters like `@` or `:` is never mis-parsed.
type PostgresDSN struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
	SSLMode  string // libpq sslmode from the URL query, e.g. "disable" (may be empty)
}

func Load() Config {
	// Load .env if present; real environment variables always win, and a
	// missing file is fine (the deploy compose injects the environment directly).
	_ = godotenv.Load()

	databaseURL := env("DATABASE_URL", "postgres://venapce:venapce@localhost:5432/venapce?sslmode=disable")

	// Superset reaches Postgres over its own network, so its view of the host can
	// differ from the API's. Default every field to what the API uses (parsed from
	// DATABASE_URL) but allow per-field overrides — the common one being the host,
	// since `localhost` from a Superset container is the container, not this host.
	dsn := parsePostgresDSN(databaseURL)
	dsn.Host = env("SUPERSET_DB_HOST", dsn.Host)
	dsn.Port = envInt("SUPERSET_DB_PORT", dsn.Port)
	dsn.Database = env("SUPERSET_DB_DATABASE", dsn.Database)
	dsn.Username = env("SUPERSET_DB_USER", dsn.Username)
	dsn.Password = env("SUPERSET_DB_PASSWORD", dsn.Password)
	dsn.SSLMode = env("SUPERSET_DB_SSLMODE", dsn.SSLMode)

	return Config{
		Port:        env("PORT", "8080"),
		DatabaseURL: databaseURL,
		AppSecret:   env("APP_SECRET_KEY", "dev-insecure-change-me"),
		CORSOrigins: env("CORS_ORIGINS", "http://localhost:5173"),

		SupersetDBName: env("SUPERSET_DB_NAME", "Venapce"),
		SupersetSchema: env("SUPERSET_SCHEMA", "public"),
		SupersetDB:     dsn,
	}
}

// parsePostgresDSN breaks DATABASE_URL into discrete connection fields. url.Parse
// correctly splits userinfo on the last '@'
func parsePostgresDSN(dbURL string) PostgresDSN {
	u, err := url.Parse(dbURL)
	if err != nil {
		return PostgresDSN{}
	}
	dsn := PostgresDSN{
		Host:     u.Hostname(),
		Port:     5432,
		Database: strings.TrimPrefix(u.Path, "/"),
		SSLMode:  u.Query().Get("sslmode"),
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			dsn.Port = n
		}
	}
	if u.User != nil {
		dsn.Username = u.User.Username()
		if pass, ok := u.User.Password(); ok {
			dsn.Password = pass
		}
	}
	return dsn
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
