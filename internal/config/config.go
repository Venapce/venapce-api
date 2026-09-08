package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// infraNatsPort is the NATS port infra serves plugins on. The osspace HTTP API
// (:8022) is derived from the same host on the venapce side (see deriveInfraBase).
const infraNatsPort = "4222"

// Config is the process configuration, all sourced from the environment so the
// same binary runs in the deploy compose (DATABASE_URL=...@postgres:5432/venapce)
// and locally.
type Config struct {
	Port        string
	DatabaseURL string
	AppSecret   string // key material for encrypting stored secrets (Superset password)
	CORSOrigins string // comma-separated allowed origins for the browser front

	// FloMorphic API access. Venapce runs as a FloMorphic plugin; rather than the
	// operator pasting a plugin env, the backend calls the FloMorphic API to mint
	// its own runtime credential (POST /extension/plugin/cred). FlomorphicURL is
	// the FloMorphic API base; FlomorphicJWTSecret is the HS256 secret the backend
	// signs an admin bearer token with. Both must be set for the plugin flow.
	FlomorphicURL       string
	FlomorphicJWTSecret string

	// InfraHost is the hostname of the infra service. In the deploy compose it is
	// the infra container name; in local dev the developer sets it. Venapce uses it
	// for both infra ports: the plugin connects to NATS on :4222, and osctrl-space
	// requests hit the osspace HTTP API on :8022 (derived from the same host). It is
	// passed as INFRA_URL when minting the plugin credential, so the minted env
	// carries the host venapce can actually reach infra on.
	InfraHost string

	// Superset provisioning: on boot we lazily register the venapce Postgres as a
	// Superset database connection and add the data tables (stage, issues) as
	// datasets, so charts can be built against them without manual setup. The
	// connection details are taken from DATABASE_URL — the very same Postgres the
	// API connects to — so there's nothing extra to configure.
	SupersetDBName string      // display name of the Superset database connection
	SupersetSchema string      // schema the data tables live in
	SupersetDB     PostgresDSN // discrete connection fields for Superset's add-database form

	// Superset admin connection, injected by the deploy compose (the very same
	// credentials it bootstraps Superset's admin with). When all three are set the
	// backend auto-configures the Superset connection on boot — the operator never
	// types them in Settings. An advanced operator can still override the stored
	// connection with an external Superset, at which point it becomes theirs to
	// manage and env no longer touches it. See SupersetManaged.
	SupersetURL       string // internal Superset base URL, e.g. http://superset:8088
	SupersetAdminUser string
	SupersetAdminPass string
}

// FlomorphicConfigured reports whether the environment specifies the FloMorphic
// API access needed to mint a plugin credential (URL + signing secret).
func (c Config) FlomorphicConfigured() bool {
	return c.FlomorphicURL != "" && c.FlomorphicJWTSecret != ""
}

// InfraNatsURL is the plugin's INFRA_URL built from InfraHost — nats://host:4222.
// Passed as an INFRA_URL extra when minting the plugin credential so the returned
// env points the plugin (and, via deriveInfraBase, osspace) at the reachable host.
// Empty when InfraHost is unset.
func (c Config) InfraNatsURL() string { return InfraNatsURL(c.InfraHost) }

// InfraNatsURL builds a plugin INFRA_URL (nats://host:4222) for an arbitrary host,
// or "" when host is empty. Package-level as well as a Config method because the
// host can be overridden from Settings at runtime, not just by this process's
// environment.
func InfraNatsURL(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	return "nats://" + net.JoinHostPort(host, infraNatsPort)
}

// SupersetManaged reports whether the environment fully specifies the built-in
// Superset connection, so the backend can self-configure it on boot.
func (c Config) SupersetManaged() bool {
	return c.SupersetURL != "" && c.SupersetAdminUser != "" && c.SupersetAdminPass != ""
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

		FlomorphicURL:       strings.TrimRight(env("FLOMORPHIC_URL", ""), "/"),
		FlomorphicJWTSecret: env("FLOMORPHIC_JWT_SECRET", ""),

		InfraHost: env("INFRA_HOST", "infra"),

		SupersetDBName: env("SUPERSET_DB_NAME", "Venapce"),
		SupersetSchema: env("SUPERSET_SCHEMA", "public"),
		SupersetDB:     dsn,

		SupersetURL:       env("SUPERSET_URL", ""),
		SupersetAdminUser: env("SUPERSET_ADMIN_USER", ""),
		SupersetAdminPass: env("SUPERSET_ADMIN_PASS", ""),
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
