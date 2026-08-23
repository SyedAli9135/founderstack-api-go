package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	coherecli "github.com/cohere-ai/cohere-go/v2/client"
	coreoption "github.com/cohere-ai/cohere-go/v2/option"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/documents"
	"github.com/founderstack/api/internal/api/identity"
	integrationsapi "github.com/founderstack/api/internal/api/integrations"
	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/settings"
	v1 "github.com/founderstack/api/internal/api/v1"
	"github.com/founderstack/api/internal/api/webhooks"
	"github.com/founderstack/api/internal/config"
	coredocs "github.com/founderstack/api/internal/core/documents"
	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/core/integrations/providers"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	mcpservers "github.com/founderstack/api/internal/core/mcp/servers"
	"github.com/founderstack/api/internal/pkg/vault"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Configures the Clerk SDK's default backend (used by
	// middleware.RequireAuth to fetch JWKS for JWT verification).
	clerk.SetKey(cfg.ClerkSecretKey.Expose())

	// Decoded once at startup so a misconfigured ENCRYPTION_KEY fails the
	// process at boot — not silently, on a founder's first BYOK submission.
	encryptionKey, err := vault.DecodeKey(cfg.EncryptionKey.Expose())
	if err != nil {
		return fmt.Errorf("decode ENCRYPTION_KEY: %w", err)
	}

	// Cancelled on SIGINT/SIGTERM; everything below shuts down off this.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Connects as app_user (RLS-enforced), not the postgres superuser that
	// runs migrations — see config.go's AppDatabaseURL doc comment.
	dbPool, err := pgxpool.New(ctx, cfg.AppDatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer dbPool.Close()

	// Connects as app_system (BYPASSRLS) — for the Clerk webhook and any
	// future system context that legitimately spans tenants. See
	// config.go's SystemDatabaseURL doc comment.
	systemPool, err := pgxpool.New(ctx, cfg.SystemDatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres (system pool): %w", err)
	}
	defer systemPool.Close()

	redisClient, err := newRedisClient(cfg)
	if err != nil {
		return fmt.Errorf("configure redis client: %w", err)
	}
	defer redisClient.Close()

	pineconeClient, err := newPineconeClient(cfg)
	if err != nil {
		return fmt.Errorf("configure pinecone client: %w", err)
	}

	integrationsRegistry := newIntegrationsRegistry(cfg)

	// Not wired to any HTTP route or Gateway yet — tool execution is
	// workflow 9's job (the executor node, once internal/core/graph
	// exists; that's also when coremcp.NewGateway(dbPool, encryptionKey,
	// mcpRegistry) actually gets called). Building the registry here
	// anyway means a wiring bug (a malformed tool schema, a missing
	// registration) fails the process at boot, not silently the first
	// time something tries to call a tool.
	mcpRegistry, err := newMCPRegistry(ctx)
	if err != nil {
		return fmt.Errorf("build mcp tool registry: %w", err)
	}
	if tools, err := mcpRegistry.ListTools(ctx); err == nil {
		count := 0
		for _, ts := range tools {
			count += len(ts)
		}
		logger.Info("mcp tool registry ready", "services", len(tools), "tools", count)
	}

	// Workflow 6 (document upload / RAG). Pinecone is required here (unlike
	// newPineconeClient's nil-if-unconfigured fallback for the health
	// check) — a document upload with no vector store to index into isn't
	// a degraded mode, it's a broken one, so a missing PINECONE_API_KEY
	// fails boot via the required-fields check in config.Load, and this
	// constructor assumes pineconeClient is non-nil.
	docsStore, err := coredocs.NewStore(ctx, cfg.AWSRegion, cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey.Expose(), cfg.AWSS3EndpointURL, cfg.S3BucketDocuments)
	if err != nil {
		return fmt.Errorf("build documents s3 store: %w", err)
	}
	docsProcessor, err := newDocumentsProcessor(ctx, cfg, dbPool, docsStore, pineconeClient)
	if err != nil {
		return fmt.Errorf("build documents processor: %w", err)
	}

	// One-time sweep for any document whose processing/purge goroutine was
	// lost to a prior process restart — see RecoverStuckJobs's doc comment
	// for why this is the accepted tradeoff of goroutines over a durable
	// job queue.
	docsProcessor.RecoverStuckJobs(ctx, systemPool)

	router := newRouter(cfg, dbPool, systemPool, redisClient, pineconeClient, encryptionKey, integrationsRegistry, docsStore, docsProcessor)

	// Runs until ctx is cancelled (same SIGINT/SIGTERM signal the HTTP
	// server shuts down on) — see integrations.RunRefreshJob's doc comment
	// for why this needs systemPool rather than the RLS-scoped dbPool.
	go integrations.RunRefreshJob(ctx, systemPool, encryptionKey, integrationsRegistry)

	srv := &http.Server{
		Addr:              addr(),
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("starting FounderStack API", "addr", srv.Addr, "env", cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down FounderStack API")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

func addr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8000"
}

func newRedisClient(cfg *config.Config) (*redis.Client, error) {
	redisURL := cfg.UpstashRedisURL
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	if !cfg.UpstashRedisToken.IsEmpty() {
		opts.Password = cfg.UpstashRedisToken.Expose()
	}
	return redis.NewClient(opts), nil
}

// newPineconeClient returns nil (not an error) when no Pinecone key is
// configured — Pinecone is optional for the API to boot locally, and the
// health check reports that state explicitly rather than failing startup.
func newPineconeClient(cfg *config.Config) (*pinecone.Client, error) {
	if cfg.PineconeAPIKey.IsEmpty() {
		return nil, nil
	}
	client, err := pinecone.NewClient(pinecone.NewClientParams{
		ApiKey:    cfg.PineconeAPIKey.Expose(),
		SourceTag: "founderstack-api-go",
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

func newRouter(cfg *config.Config, db, systemDB *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client, encryptionKey []byte, registry *integrations.Registry, docsStore *coredocs.Store, docsProcessor *coredocs.Processor) *gin.Engine {
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(middleware.RequestID())
	router.Use(middleware.Recovery(cfg))
	router.Use(cors.New(corsConfig(cfg)))

	apiV1 := router.Group("/api/v1")
	v1.NewHealthHandler(db, rdb, pc).Register(apiV1)

	apiAuth := router.Group("/api/v1/auth")
	identity.NewDevTokenHandler(cfg).Register(apiAuth)

	// Every route under here requires a verified session — db is app_user
	// (RLS-enforced); each handler scopes its own queries via tenant.WithTx.
	apiSettings := router.Group("/api/v1/settings")
	apiSettings.Use(middleware.RequireAuth(systemDB, cfg))
	settings.NewHandler(db, encryptionKey, cfg.APIKeyMockPrefix).Register(apiSettings)

	apiWebhooks := router.Group("/api/webhooks")
	webhooks.NewClerkHandler(systemDB, cfg.ClerkWebhookSecret.Expose()).Register(apiWebhooks)

	stateManager := integrations.NewStateManager(rdb, cfg.OAuthStateSecret.Expose())
	intHandler := integrationsapi.NewHandler(db, encryptionKey, registry, stateManager, cfg.FrontendURL)

	apiIntegrations := router.Group("/api/v1/integrations")
	apiIntegrations.Use(middleware.RequireAuth(systemDB, cfg))
	intHandler.Register(apiIntegrations)

	// Unauthenticated: the OAuth provider redirects the founder's browser
	// here directly, with no JWT — see Handler.RegisterCallback's doc
	// comment. Same URL prefix as apiIntegrations above; the route
	// patterns (":service/callback" vs. ":service/connect" etc.) don't
	// collide, so Gin's router handles both groups fine.
	apiIntegrationsCallback := router.Group("/api/v1/integrations")
	intHandler.RegisterCallback(apiIntegrationsCallback)

	apiDocuments := router.Group("/api/v1")
	apiDocuments.Use(middleware.RequireAuth(systemDB, cfg))
	documents.NewHandler(db, docsStore, docsProcessor).Register(apiDocuments)

	return router
}

// newIntegrationsRegistry constructs one provider per catalog.go entry —
// the single place, per the "open/closed" design note in
// WORKFLOW_PLAN_GO.md's workflow 4 section, that needs a new line when a
// provider is added. Every provider's redirect URL follows the same
// {API_URL}/api/v1/integrations/{service}/callback shape.
func newIntegrationsRegistry(cfg *config.Config) *integrations.Registry {
	callbackURL := func(service string) string {
		return cfg.AppBaseURL + "/api/v1/integrations/" + service + "/callback"
	}
	return integrations.NewRegistry(
		providers.NewSlack(cfg.SlackClientID, cfg.SlackClientSecret.Expose(), callbackURL("slack")),
		providers.NewDiscord(cfg.DiscordClientID, cfg.DiscordClientSecret.Expose(), callbackURL("discord")),
		providers.NewNotion(cfg.NotionClientID, cfg.NotionClientSecret.Expose(), callbackURL("notion")),
		providers.NewGoogleDrive(cfg.GoogleClientID, cfg.GoogleClientSecret.Expose(), callbackURL("google_drive")),
		providers.NewGoogleCalendar(cfg.GoogleClientID, cfg.GoogleClientSecret.Expose(), callbackURL("google_calendar")),
		providers.NewLinkedIn(cfg.LinkedInClientID, cfg.LinkedInClientSecret.Expose(), callbackURL("linkedin")),
		providers.NewStripe(),
		providers.NewGitHub(),
	)
}

// newMCPRegistry connects every MCP tool server (workflow 5) —
// mcpservers.AllServers() is the single place a new tool server needs a
// new line (shared with cmd/seedtools, which needs the identical map).
// Every server is wired via mcp.NewInMemoryTransports() inside
// coremcp.NewRegistry, not called directly — see internal/core/mcp's
// package doc for why.
func newMCPRegistry(ctx context.Context) (*coremcp.Registry, error) {
	return coremcp.NewRegistry(ctx, mcpservers.AllServers())
}

// newDocumentsProcessor builds workflow 6's Cohere + Pinecone-backed
// Processor. pineconeClient is assumed non-nil — see its call site's
// comment for why documents treats Pinecone as required rather than
// optional the way the health check does.
func newDocumentsProcessor(ctx context.Context, cfg *config.Config, appPool *pgxpool.Pool, store *coredocs.Store, pineconeClient *pinecone.Client) (*coredocs.Processor, error) {
	// WithMaxAttempts(7): found live, not from Cohere's docs — a small test
	// upload (28 chunks, one partial batch) embedded fine, but a real 27MB
	// PDF (480+ chunks, several full batches) started failing partway
	// through with Cohere's trial-tier "100000 tokens per minute" 429. The
	// SDK's own retrier (internal/retrier.go) already backs off
	// exponentially (1s/2s/4s/8s/16s/32s, capped at 60s, ±10% jitter,
	// Retry-After-aware) but only retries twice by default — nowhere near
	// enough to survive a per-minute rate-limit window. 7 attempts gives
	// ~63s of cumulative backoff, comfortably covering one full window
	// reset without failing the whole document over a transient cap.
	cohereClient := coherecli.NewClient(coreoption.WithToken(cfg.CohereAPIKey.Expose()), coreoption.WithMaxAttempts(7))

	idx, err := pineconeClient.DescribeIndex(ctx, cfg.PineconeIndexRAG)
	if err != nil {
		return nil, fmt.Errorf("describe pinecone rag index %q: %w", cfg.PineconeIndexRAG, err)
	}
	idxConn, err := pineconeClient.Index(pinecone.NewIndexConnParams{Host: idx.Host})
	if err != nil {
		return nil, fmt.Errorf("connect to pinecone rag index: %w", err)
	}

	return coredocs.NewProcessor(appPool, store, coredocs.NewCohereEmbedder(cohereClient), coredocs.NewPineconeIndex(idxConn)), nil
}

// corsConfig mirrors app/main.py's CORS policy: wide open in development,
// locked to the app's own origins in production. AllowOriginFunc (rather
// than the literal AllowOrigins: []string{"*"}) is used for the dev case
// because the CORS spec forbids combining a wildcard origin with
// AllowCredentials — the browser will reject "*" + credentials outright.
func corsConfig(cfg *config.Config) cors.Config {
	c := cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Accept"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	if cfg.IsProduction() {
		c.AllowOrigins = []string{cfg.AppBaseURL, "https://founderstack.ai"}
	} else {
		c.AllowOriginFunc = func(origin string) bool { return true }
	}
	return c
}
