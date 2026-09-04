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

	"github.com/founderstack/api/internal/api/agents"
	approvalsapi "github.com/founderstack/api/internal/api/approvals"
	"github.com/founderstack/api/internal/api/documents"
	"github.com/founderstack/api/internal/api/identity"
	integrationsapi "github.com/founderstack/api/internal/api/integrations"
	"github.com/founderstack/api/internal/api/middleware"
	runsapi "github.com/founderstack/api/internal/api/runs"
	"github.com/founderstack/api/internal/api/settings"
	v1 "github.com/founderstack/api/internal/api/v1"
	"github.com/founderstack/api/internal/api/webhooks"
	workflowsapi "github.com/founderstack/api/internal/api/workflows"
	"github.com/founderstack/api/internal/config"
	coredocs "github.com/founderstack/api/internal/core/documents"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/core/integrations/providers"
	corellm "github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	mcpservers "github.com/founderstack/api/internal/core/mcp/servers"
	"github.com/founderstack/api/internal/core/notify"
	coreworkflows "github.com/founderstack/api/internal/core/workflows"
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

	// Used by middleware.RequireAuth to fetch JWKS for JWT verification.
	clerk.SetKey(cfg.ClerkSecretKey.Expose())

	// Decoded once at startup so a misconfigured ENCRYPTION_KEY fails the
	// process at boot, not silently on a founder's first BYOK submission.
	encryptionKey, err := vault.DecodeKey(cfg.EncryptionKey.Expose())
	if err != nil {
		return fmt.Errorf("decode ENCRYPTION_KEY: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// app_user, RLS-enforced — not the postgres superuser that runs migrations.
	dbPool, err := pgxpool.New(ctx, cfg.AppDatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer dbPool.Close()

	// app_system, BYPASSRLS — the Clerk webhook and any system context
	// that legitimately spans tenants.
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

	// Built at boot so a wiring bug (a malformed tool schema, a missing
	// registration) fails the process here, not the first tool call.
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

	// mcpGateway fetches+decrypts an org's integration token and attaches
	// it to a tool call. graphEngine is a singleton shared by every run
	// this process starts — its EventBus/cancel map are process-wide by
	// design.
	mcpGateway := coremcp.NewGateway(dbPool, encryptionKey, mcpRegistry, redisClient)
	graphEngine := graph.NewEngine(dbPool)

	// Each notify channel degrades to a logged no-op when its config is
	// unset, so notifier is never nil and safe to use without a further
	// nil check.
	actionTokens := notify.NewActionTokenSigner(cfg.PushActionTokenSecret)
	notifier := notify.New(
		notify.NewEmailSender(cfg.BrevoAPIKey, cfg.BrevoFromEmail),
		notify.NewWebPushSender(cfg.WebPushVAPIDPublicKey, cfg.WebPushVAPIDPrivateKey, cfg.WebPushVAPIDSubject),
		actionTokens,
		cfg.AppBaseURL,
	)

	var launcher *graph.Launcher
	if cfg.MockLLMMode {
		// A misconfigured MOCK_LLM_MODE=true in production must fail loud
		// at boot, not silently run every agent against canned responses.
		if cfg.IsProduction() {
			return fmt.Errorf("MOCK_LLM_MODE=true is not allowed when APP_ENV=production")
		}
		logger.Warn("MOCK_LLM_MODE enabled — every workflow run will use a scripted MockChatClient, no real LLM provider will be called")
		launcher = graph.NewLauncherWithResolver(graphEngine, dbPool, encryptionKey, mcpRegistry, mcpGateway, notifier, corellm.MockScenarioResolver)
	} else {
		launcher = graph.NewLauncher(graphEngine, dbPool, encryptionKey, mcpRegistry, mcpGateway, notifier)
	}

	// Unlike newPineconeClient's nil-if-unconfigured fallback for the
	// health check, Pinecone is required here — a document upload with no
	// vector store to index into is broken, not degraded, so a missing
	// PINECONE_API_KEY fails boot via config.Load's required-fields check.
	docsStore, err := coredocs.NewStore(ctx, cfg.AWSRegion, cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey.Expose(), cfg.AWSS3EndpointURL, cfg.S3BucketDocuments)
	if err != nil {
		return fmt.Errorf("build documents s3 store: %w", err)
	}
	docsProcessor, err := newDocumentsProcessor(ctx, cfg, dbPool, docsStore, pineconeClient)
	if err != nil {
		return fmt.Errorf("build documents processor: %w", err)
	}

	// Re-dispatches any document whose processing/purge goroutine was lost
	// to a prior process restart.
	docsProcessor.RecoverStuckJobs(ctx, systemPool)

	router := newRouter(cfg, dbPool, systemPool, redisClient, pineconeClient, encryptionKey, integrationsRegistry, docsStore, docsProcessor, mcpRegistry, graphEngine, launcher, actionTokens)

	// All 3 background jobs run on systemPool (BYPASSRLS) — each scans
	// across every org, which is inherently cross-tenant — and stop when
	// ctx is cancelled by the same SIGINT/SIGTERM the HTTP server shuts
	// down on.
	go integrations.RunRefreshJob(ctx, systemPool, encryptionKey, integrationsRegistry)
	go coreworkflows.RunScheduler(ctx, systemPool)
	go coreworkflows.RunApprovalExpiryJob(ctx, systemPool, launcher)

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
// configured — the health check reports that state rather than failing boot.
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

func newRouter(cfg *config.Config, db, systemDB *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client, encryptionKey []byte, registry *integrations.Registry, docsStore *coredocs.Store, docsProcessor *coredocs.Processor, mcpRegistry *coremcp.Registry, graphEngine *graph.Engine, launcher *graph.Launcher, actionTokens *notify.ActionTokenSigner) *gin.Engine {
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

	// Every route under here requires a verified session; each handler
	// scopes its own queries via tenant.WithTx against the app_user pool.
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
	// here directly, with no JWT. Same URL prefix as apiIntegrations above;
	// the route patterns don't collide.
	apiIntegrationsCallback := router.Group("/api/v1/integrations")
	intHandler.RegisterCallback(apiIntegrationsCallback)

	apiDocuments := router.Group("/api/v1")
	apiDocuments.Use(middleware.RequireAuth(systemDB, cfg))
	documents.NewHandler(db, docsStore, docsProcessor).Register(apiDocuments)

	apiAgents := router.Group("/api/v1")
	apiAgents.Use(middleware.RequireAuth(systemDB, cfg))
	agents.NewHandler(db, mcpRegistry).Register(apiAgents)

	apiWorkflows := router.Group("/api/v1")
	apiWorkflows.Use(middleware.RequireAuth(systemDB, cfg))
	workflowsapi.NewHandler(db, launcher).Register(apiWorkflows)

	apiRuns := router.Group("/api/v1")
	apiRuns.Use(middleware.RequireAuth(systemDB, cfg))
	runsapi.NewHandler(db, graphEngine).Register(apiRuns)

	// Dev-only stand-in for the real approve/reject endpoints, registered
	// only when MOCK_LLM_MODE is on.
	if cfg.MockLLMMode {
		runsapi.NewDevResumeHandler(db, launcher).Register(apiRuns)
	}

	approvalsHandler := approvalsapi.NewHandler(db, systemDB, launcher, actionTokens, cfg)

	apiApprovals := router.Group("/api/v1")
	apiApprovals.Use(middleware.RequireAuth(systemDB, cfg))
	approvalsHandler.Register(apiApprovals)

	// Deliberately ungated — a push notification's Approve/Reject buttons
	// have no live Clerk session; Handler.resolveActor does its own dual
	// auth (Bearer or ?action_token=) per request instead.
	apiApprovalsActions := router.Group("/api/v1")
	approvalsHandler.RegisterActions(apiApprovalsActions)

	return router
}

// newIntegrationsRegistry constructs one provider per catalog.go entry —
// the single place a new provider needs a new line. Every provider's
// redirect URL follows the same {API_URL}/.../{service}/callback shape.
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

// newMCPRegistry connects every MCP tool server — mcpservers.AllServers()
// is the single place a new tool server needs a new line (shared with
// cmd/seedtools). Wired via mcp.NewInMemoryTransports() inside
// coremcp.NewRegistry, not called directly.
func newMCPRegistry(ctx context.Context) (*coremcp.Registry, error) {
	return coremcp.NewRegistry(ctx, mcpservers.AllServers())
}

// newDocumentsProcessor builds the Cohere + Pinecone-backed Processor.
// pineconeClient is assumed non-nil.
func newDocumentsProcessor(ctx context.Context, cfg *config.Config, appPool *pgxpool.Pool, store *coredocs.Store, pineconeClient *pinecone.Client) (*coredocs.Processor, error) {
	// WithMaxAttempts(7): a large (480+ chunk) document can outlast
	// Cohere's own retrier's default 2 attempts against its per-minute
	// rate limit — 7 gives ~63s of cumulative backoff, enough to cover one
	// full window reset.
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

// corsConfig is wide open in development, locked to the app's own origins
// in production. AllowOriginFunc, not AllowOrigins: []string{"*"}, for the
// dev case — the CORS spec forbids combining a wildcard origin with
// AllowCredentials.
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
