// Package config reads runtime settings from environment variables.
//
// Only infrastructure settings live here (DB URL, HTTP port).
// PLC and PV configuration lives in PostgreSQL — never here.
package config

import (
	"bufio"
	"os"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL    string        // DATABASE_URL
	HTTPAddr       string        // HTTP_ADDR
	ConfigReload   time.Duration // CONFIG_RELOAD — how often acquisition re-reads plc/pv metadata
	AcquireEnabled bool          // ACQUISITION — "off" runs the web UI only
}

func Load() Config {
	// A .env file next to the executable is convenient on Windows.
	// Real environment variables always win over .env values.
	loadDotEnv(".env")

	return Config{
		DatabaseURL:    env("DATABASE_URL", "postgres://postgres@localhost:5432/plc_monitoring"),
		HTTPAddr:       env("HTTP_ADDR", ":8080"),
		ConfigReload:   envDuration("CONFIG_RELOAD", 10*time.Second),
		AcquireEnabled: env("ACQUISITION", "on") != "off",
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(env(key, ""))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// loadDotEnv sets KEY=VALUE lines from path unless the variable is already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env is fine
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
