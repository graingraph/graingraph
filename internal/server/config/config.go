// Package config defines the runtime configuration of the graingraph-query server.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the graingraph-query server version reported by discovery and -version.
const Version = "0.4.0"

// Config holds all runtime settings for the server.
type Config struct {
	// ListenAddr is the HTTP listen address, e.g. ":7474".
	ListenAddr string
	// DBPath is the graingraph (SQLite) database file. ":memory:" opens a
	// transient in-memory database.
	DBPath string
	// DatabaseName is the name advertised in the discovery response. The
	// <databaseName> path segment of requests is accepted for any value;
	// graingraph is a single embedded database.
	DatabaseName string
	// ReadOnly opens the graingraph database in read-only mode.
	ReadOnly bool
	// TxTimeout is the idle timeout applied to explicit transactions.
	// Transactions idle for longer than this are rolled back by a reaper.
	// Zero disables the reaper.
	TxTimeout time.Duration
	// ImportDir, when set, enables LOAD CSV restricted to files below this
	// directory. Empty disables LOAD CSV.
	ImportDir string
	// Username and Password are the single basic-auth credential.
	Username string
	Password string
	// TLSCert and TLSKey enable TLS when both are non-empty.
	TLSCert string
	TLSKey  string
	// PublicURL, when set, is the externally visible base URL advertised
	// by the discovery endpoint, e.g. "https://graph.example.com". When
	// empty it is derived from each request's Host header.
	PublicURL string
}

// Validate checks the configuration is usable. The server must not start
// without a basic-auth username and password.
func (c Config) Validate() error {
	if c.Username == "" || c.Password == "" {
		return errors.New("no basic-auth credentials configured; set --username/--password or GRAINGRAPH_QUERY_USERNAME/GRAINGRAPH_QUERY_PASSWORD")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("--tls-cert and --tls-key must be set together")
	}
	if c.TxTimeout < 0 {
		return errors.New("--tx-timeout must not be negative")
	}
	if c.ListenAddr == "" {
		return errors.New("--listen must not be empty")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("--public-url must be an absolute http(s) URL, got %q", c.PublicURL)
		}
	}
	return nil
}

// EnvOr returns the value of environment variable key, or def when unset
// or empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvBool returns the boolean value of environment variable key, or def when
// unset or not a recognised boolean.
func EnvBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "t", "true", "yes", "on":
		return true
	case "0", "f", "false", "no", "off":
		return false
	}
	return def
}

// EnvDuration returns the duration value of environment variable key, or def
// when unset or not a valid duration.
func EnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// EnvInt returns the integer value of environment variable key, or def when
// unset or not a recognised integer.
func EnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// EnvInt64 returns the 64-bit integer value of environment variable key, or def
// when unset or not a recognised integer.
func EnvInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// SplitList splits a comma-separated list, trimming blanks. "none" yields
// an empty list.
func SplitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(p, "none") {
			out = append(out, p)
		}
	}
	return out
}
