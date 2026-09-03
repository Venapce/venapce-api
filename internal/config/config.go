package config

import (
	"os"

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
}

func Load() Config {
	// Load .env if present; real environment variables always win, and a
	// missing file is fine (the deploy compose injects the environment directly).
	_ = godotenv.Load()

	return Config{
		Port:        env("PORT", "8080"),
		DatabaseURL: env("DATABASE_URL", "postgres://venapce:venapce@localhost:5432/venapce?sslmode=disable"),
		AppSecret:   env("APP_SECRET_KEY", "dev-insecure-change-me"),
		CORSOrigins: env("CORS_ORIGINS", "http://localhost:5173"),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
