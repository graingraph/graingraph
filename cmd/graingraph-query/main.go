// Command graingraph-query serves an HTTP API with the Neo4j Query API
// contract on top of an embedded graingraph graph database.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/graingraph/graingraph"
	"github.com/graingraph/graingraph/internal/server/api"
	"github.com/graingraph/graingraph/internal/server/auth"
	"github.com/graingraph/graingraph/internal/server/config"
	"github.com/graingraph/graingraph/internal/server/store"
)

// shutdownTimeout bounds how long in-flight requests get to finish once
// shutdown starts.
var shutdownTimeout = 15 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, nil)
	stop()
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case isUsageError(err):
		os.Exit(2)
	default:
		slog.Error("graingraph-query exited", "error", err)
		os.Exit(1)
	}
}

// usageError marks a command-line parsing failure, already reported by
// the flag package.
type usageError struct{ error }

func isUsageError(err error) bool {
	var u usageError
	return errors.As(err, &u)
}

// setupLogging installs the default slog logger and returns a function
// that closes the log file, if any.
func setupLogging(level, format, file string) (func(), error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: must be debug, info, warn or error", level)
	}
	out := io.Writer(os.Stderr)
	closeFn := func() {}
	if file != "" {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		out, closeFn = f, func() { _ = f.Close() }
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(out, opts)
	case "json":
		h = slog.NewJSONHandler(out, opts)
	default:
		closeFn()
		return nil, fmt.Errorf("--log-format %q: must be text or json", format)
	}
	slog.SetDefault(slog.New(h))
	return closeFn, nil
}

// run starts the server and blocks until ctx is cancelled (graceful
// shutdown, returns nil) or serving fails. ready, if non-nil, is called
// with the bound address once the server accepts connections.
func run(ctx context.Context, args []string, stdout io.Writer, ready func(addr string)) error {
	if len(args) > 0 && args[0] == "skill" {
		return runSkill(args[1:], stdout)
	}
	flags := flag.NewFlagSet("graingraph-query", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var (
		listen    = flags.String("listen", config.EnvOr("GRAINGRAPH_QUERY_LISTEN", ":7474"), "HTTP listen address")
		dbPath    = flags.String("db", config.EnvOr("GRAINGRAPH_QUERY_DB", "graingraph.db"), `path to the graingraph SQLite database (":memory:" for a transient store)`)
		database  = flags.String("database-name", config.EnvOr("GRAINGRAPH_QUERY_DATABASE_NAME", "neo4j"), "database name advertised in discovery")
		readOnly  = flags.Bool("read-only", config.EnvBool("GRAINGRAPH_QUERY_READ_ONLY", false), "open the database read-only")
		txTimeout = flags.Duration("tx-timeout", config.EnvDuration("GRAINGRAPH_QUERY_TX_TIMEOUT", 5*time.Minute), "idle timeout for explicit transactions (0 disables)")
		txWait    = flags.Duration("tx-wait", config.EnvDuration("GRAINGRAPH_QUERY_TX_WAIT", 5*time.Second), "how long a request waits for an open transaction to end before failing (0 fails at once)")
		pageCache = flags.Int64("page-cache", config.EnvInt64("GRAINGRAPH_QUERY_PAGE_CACHE", 0), "SQLite page cache in bytes (raises PRAGMA cache_size); 0 uses the ~2 MiB default")
		mmapSize  = flags.Int64("mmap-size", config.EnvInt64("GRAINGRAPH_QUERY_MMAP_SIZE", 0), "memory-mapped I/O of the database file, in bytes; a negative value disables it, 0 uses SQLite's default")
		syncModeS = flags.String("synchronous", config.EnvOr("GRAINGRAPH_QUERY_SYNCHRONOUS", ""), "PRAGMA synchronous: full, normal or off; blank leaves the journal_mode default (full)")
		pageSize  = flags.Int("page-size", config.EnvInt("GRAINGRAPH_QUERY_PAGE_SIZE", 0), "page size in bytes for a new database file; a power of two between 512 and 32768")
		importDir = flags.String("import-dir", config.EnvOr("GRAINGRAPH_QUERY_IMPORT_DIR", ""), "directory that enables LOAD CSV, restricted to files below it")
		username  = flags.String("username", config.EnvOr("GRAINGRAPH_QUERY_USERNAME", ""), "basic-auth username")
		password  = flags.String("password", config.EnvOr("GRAINGRAPH_QUERY_PASSWORD", ""), "basic-auth password (visible in the process list; prefer GRAINGRAPH_QUERY_PASSWORD)")
		tlsCert   = flags.String("tls-cert", config.EnvOr("GRAINGRAPH_QUERY_TLS_CERT", ""), "TLS certificate file")
		tlsKey    = flags.String("tls-key", config.EnvOr("GRAINGRAPH_QUERY_TLS_KEY", ""), "TLS private key file")
		publicURL = flags.String("public-url", config.EnvOr("GRAINGRAPH_QUERY_PUBLIC_URL", ""), "externally visible base URL advertised by discovery (default: derived from the request)")
		logLevel  = flags.String("log-level", config.EnvOr("GRAINGRAPH_QUERY_LOG_LEVEL", "info"), "log level: debug, info, warn or error")
		logFormat = flags.String("log-format", config.EnvOr("GRAINGRAPH_QUERY_LOG_FORMAT", "text"), "log format: text or json")
		logFile   = flags.String("log-file", config.EnvOr("GRAINGRAPH_QUERY_LOG_FILE", ""), "append logs to this file instead of stderr")
		logStmts  = flags.Bool("log-statements", config.EnvBool("GRAINGRAPH_QUERY_LOG_STATEMENTS", true), "include Cypher statement text in request logs (parameters are never logged)")
		cors      = flags.String("cors-origins", config.EnvOr("GRAINGRAPH_QUERY_CORS_ORIGINS", "*"), `comma-separated origins allowed by CORS, "*" for any, "none" to disable`)
		showVer   = flags.Bool("version", false, "print version and exit")
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}

	if *showVer {
		fmt.Fprintln(stdout, "graingraph-query", config.Version)
		return nil
	}

	closeLog, err := setupLogging(*logLevel, *logFormat, *logFile)
	if err != nil {
		return err
	}
	defer closeLog()

	cfg := config.Config{
		ListenAddr:   *listen,
		DBPath:       *dbPath,
		DatabaseName: *database,
		ReadOnly:     *readOnly,
		TxTimeout:    *txTimeout,
		ImportDir:    *importDir,
		Username:     *username,
		Password:     *password,
		TLSCert:      *tlsCert,
		TLSKey:       *tlsKey,
		PublicURL:    *publicURL,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	var stOpts []store.Option
	if cfg.ReadOnly {
		stOpts = append(stOpts, store.WithReadOnly())
	}
	stOpts = append(stOpts, store.WithTxWait(*txWait))
	if cfg.ImportDir != "" {
		stOpts = append(stOpts, store.WithImportDirectory(cfg.ImportDir))
	}
	// Forward the SQLite tuning options only when set, so a server that omits one
	// leaves SQLite's default in place.
	if *pageCache > 0 {
		stOpts = append(stOpts, store.WithPageCache(*pageCache))
	}
	if *mmapSize != 0 {
		stOpts = append(stOpts, store.WithMMapSize(*mmapSize))
	}
	if sm, err := parseSyncMode(*syncModeS); err != nil {
		return err
	} else if sm != graingraph.SyncDefault {
		stOpts = append(stOpts, store.WithSyncMode(sm))
	}
	if *pageSize > 0 {
		stOpts = append(stOpts, store.WithPageSize(*pageSize))
	}
	st, err := store.New(cfg.DBPath, stOpts...)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	stopReaper := st.StartReaper(cfg.TxTimeout)
	defer stopReaper()

	h := api.NewHandler(st, auth.New(cfg.Username, cfg.Password), cfg.DatabaseName, cfg.TxTimeout)
	h.SetPublicURL(cfg.PublicURL)
	h.SetIDSeed(idSeed(cfg.DBPath))
	h.SetLogStatements(*logStmts)
	h.SetCORSOrigins(config.SplitList(*cors))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 15 * time.Second,
		// Request bodies may be large (see the API body limit) but must
		// arrive in bounded time, and idle keep-alive connections must
		// not accumulate.
		ReadTimeout: 2 * time.Minute,
		IdleTimeout: 2 * time.Minute,
		// Results are materialised before the response is written, so a
		// generous write timeout covers marshalling of very large result
		// sets without capping normal work.
		WriteTimeout: 10 * time.Minute,
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLSCert != "" {
			err = srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			err = srv.Serve(ln)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	slog.Info("listening",
		"version", config.Version, "addr", ln.Addr().String(),
		"database", cfg.DatabaseName, "file", cfg.DBPath)
	if ready != nil {
		ready(ln.Addr().String())
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "error", err)
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// idSeed is what the database IDs in SHOW DATABASES are derived from: the
// absolute path of the database file, so the IDs are stable across restarts.
// A transient in-memory store has no identity worth keeping and gets a random one.
func idSeed(dbPath string) string {
	if dbPath == ":memory:" || dbPath == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return "memory:" + hex.EncodeToString(b)
	}
	if abs, err := filepath.Abs(dbPath); err == nil {
		return abs
	}
	return dbPath
}

// parseSyncMode maps the --synchronous flag onto a graingraph.SyncMode. An empty
// value means "leave journal_mode's default" (SyncDefault). "extra" is accepted as
// an alias for full, mirroring SQLite's synchronous=EXTRA, which WAL treats the
// same as FULL here.
func parseSyncMode(s string) (graingraph.SyncMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return graingraph.SyncDefault, nil
	case "off":
		return graingraph.SyncOff, nil
	case "normal":
		return graingraph.SyncNormal, nil
	case "full", "extra":
		return graingraph.SyncFull, nil
	default:
		return graingraph.SyncDefault, fmt.Errorf("--synchronous must be full, normal or off; got %q", s)
	}
}
