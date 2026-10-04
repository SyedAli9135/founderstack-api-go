package main

import (
	"context"
	"errors"
	"fmt"
	dbpkg "github.com/founderstack/api/internal/db"
	"github.com/founderstack/api/internal/pkg/safego"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	coherecli "github.com/cohere-ai/cohere-go/v2/client"
	coreoption "github.com/cohere-ai/cohere-go/v2/option"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/clerk/clerk-sdk-go/v2/organizationinvitation"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	a2aapi "github.com/founderstack/api/internal/api/a2a"
	"github.com/founderstack/api/internal/api/agents"
	"github.com/founderstack/api/internal/api/analytics"
	approvalsapi "github.com/founderstack/api/internal/api/approvals"
	"github.com/founderstack/api/internal/api/auditlogs"
	"github.com/founderstack/api/internal/api/billing"
	"github.com/founderstack/api/internal/api/documents"
	"github.com/founderstack/api/internal/api/identity"
	integrationsapi "github.com/founderstack/api/internal/api/integrations"
	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/org"
	practiceapi "github.com/founderstack/api/internal/api/practice"
	reportsapi "github.com/founderstack/api/internal/api/reports"
	runsapi "github.com/founderstack/api/internal/api/runs"
	"github.com/founderstack/api/internal/api/settings"
	teamsapi "github.com/founderstack/api/internal/api/teams"
	templatesapi "github.com/founderstack/api/internal/api/templates"
	v1 "github.com/founderstack/api/internal/api/v1"
	"github.com/founderstack/api/internal/api/webhooks"
	workflowsapi "github.com/founderstack/api/internal/api/workflows"
	"github.com/founderstack/api/internal/config"
	corea2a "github.com/founderstack/api/internal/core/a2a"
	corebilling "github.com/founderstack/api/internal/core/billing"
	coredigest "github.com/founderstack/api/internal/core/digest"
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

	documents.SetLimits(cfg.DocsUploadsPerHour, cfg.DocsUploadMBPerHour, cfg.DocsReindexesPerHour, cfg.DocsSearchesPerMinute)
	if cfg.MaxActiveRunsPerOrg > 0 {
		graph.MaxActiveRunsPerOrg = cfg.MaxActiveRunsPerOrg
	}

	// app_user, RLS-enforced — not the postgres superuser that runs migrations.
	if cfg.IsProduction() {
		// The migrations create these roles with passwords that live in the
		// repository; a production database must not still accept them.
		if err := dbpkg.RefuseDevCredentials(ctx, cfg.SystemDatabaseURL); err != nil {
			return err
		}
	}

	dbPool, err := dbpkg.NewPool(ctx, cfg.AppDatabaseURL, int32(cfg.DatabasePoolSize))
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer dbPool.Close()

	// app_system, BYPASSRLS — the Clerk webhook and any system context
	// that legitimately spans tenants.
	// Cross-tenant sweeps and webhooks need far fewer connections than request traffic.
	systemPool, err := dbpkg.NewPool(ctx, cfg.SystemDatabaseURL, max(int32(cfg.DatabasePoolSize)/4, 4))
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
	mcpGateway.SetIntegrationRegistry(integrationsRegistry)
	graphEngine := graph.NewEngine(dbPool)
	// A run executes on one instance but its viewers (and a cancel request) can
	// reach any: relay events and cancels between instances through Redis.
	graphEngine.Bus.EnableRedis(ctx, redisClient)
	safego.Go("graph: cancel listener", func() { graphEngine.ListenForCancels(ctx) })

	// Each notify channel degrades to a logged no-op when its config is
	// unset, so notifier is never nil and safe to use without a further
	// nil check.
	actionTokens := notify.NewActionTokenSigner(cfg.PushActionTokenSecret)
	emailSender := notify.NewEmailSender(cfg.BrevoAPIKey, cfg.BrevoFromEmail)
	notifier := notify.New(
		emailSender,
		notify.NewWebPushSender(cfg.WebPushVAPIDPublicKey, cfg.WebPushVAPIDPrivateKey, cfg.WebPushVAPIDSubject),
		actionTokens,
		cfg.AppBaseURL,
	)
	digestTokens := notify.NewDigestTokenSigner(cfg.DigestUnsubscribeSecret)

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

	// Wires the orchestrator's real, loopback-HTTP A2A dispatch client.
	// A2ATaskTokenSecret unset just means every team run
	// fails cleanly at dispatch time (a2a.Client.Dispatch's own check)
	taskTokens := corea2a.NewTaskTokenSigner(cfg.A2ATaskTokenSecret)
	launcher.SetA2AClient(corea2a.NewClient(cfg.AppBaseURL, taskTokens))

	// Unlike newPineconeClient's nil-if-unconfigured fallback for the
	// health check, Pinecone is required here — a document upload with no
	// vector store to index into is broken, not degraded, so a missing
	// PINECONE_API_KEY fails boot via config.Load's required-fields check.
	docsStore, err := coredocs.NewStore(ctx, cfg.AWSRegion, cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey.Expose(), cfg.AWSS3EndpointURL, cfg.S3BucketDocuments)
	if err != nil {
		return fmt.Errorf("build documents s3 store: %w", err)
	}
	docsProcessor, docsSearcher, err := newDocumentsProcessor(ctx, cfg, dbPool, docsStore, pineconeClient)
	if err != nil {
		return fmt.Errorf("build documents processor: %w", err)
	}

	// Re-dispatches any document whose processing/purge goroutine was lost
	// to a prior process restart.
	docsProcessor.RecoverStuckJobs(ctx, systemPool)

	// Platform billing (workflow 15). Left nil — not a nil *StripeClient
	// in the interface — when unconfigured, so the handlers' nil checks work.
	var stripeAPI corebilling.Stripe
	var billingSyncer *corebilling.Syncer
	switch key := cfg.StripeSecretKey.Expose(); {
	case key == "":
		logger.Warn("STRIPE_SECRET_KEY not set — plan upgrades and the Stripe webhook are disabled")
	case strings.HasPrefix(key, "sk_live_") && !cfg.IsProduction():
		return fmt.Errorf("a live STRIPE_SECRET_KEY is not allowed outside APP_ENV=production — use an sk_test_ key")
	default:
		client := corebilling.NewStripeClient(key)
		stripeAPI = client
		billingSyncer = corebilling.NewSyncer(systemPool, client, emailSender, cfg.FrontendURL)
	}

	router := newRouter(cfg, dbPool, systemPool, redisClient, pineconeClient, encryptionKey, integrationsRegistry, docsStore, docsProcessor, docsSearcher, mcpRegistry, graphEngine, launcher, actionTokens, taskTokens, emailSender, digestTokens, stripeAPI, billingSyncer)

	// All 3 background jobs run on systemPool (BYPASSRLS) — each scans
	// across every org, which is inherently cross-tenant — and stop when
	// ctx is cancelled by the same SIGINT/SIGTERM the HTTP server shuts
	// down on.
	go integrations.RunRefreshJob(ctx, systemPool, encryptionKey, integrationsRegistry)
	go coreworkflows.RunScheduler(ctx, systemPool, launcher)
	go coreworkflows.RunApprovalExpiryJob(ctx, systemPool, launcher)
	go coredigest.RunScheduler(ctx, systemPool, emailSender, digestTokens, cfg.AppBaseURL)
	if billingSyncer != nil {
		go corebilling.RunWorkspaceUsageJob(ctx, systemPool, billingSyncer)
	}

	srv := &http.Server{
		Addr:              addr(),
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: the run event stream (SSE) is long-lived.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
	}

	// An open run stream never ends on its own, so Shutdown would wait out its
	// whole timeout for it; closing the streams first lets it finish promptly.
	srv.RegisterOnShutdown(graphEngine.Bus.CloseAll)

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
	drainInFlight(logger, graphEngine, time.Duration(cfg.ShutdownGraceSeconds)*time.Second)
	return nil
}

// drainInFlight gives runs, document jobs and notifications still executing on
// this process up to grace to finish, then cancels any run that hasn't, so it
// ends as 'cancelled' rather than sitting 'running' until the stale-run reaper
// notices an hour later. No new work can start: the server has stopped
// accepting requests.
func drainInFlight(logger *slog.Logger, engine *graph.Engine, grace time.Duration) {
	if safego.InFlight() == 0 && engine.InFlight() == 0 {
		return
	}
	logger.Info("waiting for in-flight work", "jobs", safego.InFlight(), "runs", engine.InFlight(), "grace", grace)
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := safego.Wait(ctx); err == nil {
		return
	}
	n := engine.CancelAll()
	logger.Warn("grace period over; cancelling runs still in flight", "runs", n, "jobs", safego.InFlight())
	final, cancelFinal := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinal()
	_ = safego.Wait(final)
}

// maxRequestBody caps any request body at the router. Document uploads are
// the largest legitimate body (50 MiB file cap plus multipart overhead).
const maxRequestBody = 64 << 20

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

func newRouter(cfg *config.Config, db, systemDB *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client, encryptionKey []byte, registry *integrations.Registry, docsStore *coredocs.Store, docsProcessor *coredocs.Processor, docsSearcher *coredocs.Searcher, mcpRegistry *coremcp.Registry, graphEngine *graph.Engine, launcher *graph.Launcher, actionTokens *notify.ActionTokenSigner, taskTokens *corea2a.TaskTokenSigner, emailSender notify.EmailSender, digestTokens *notify.DigestTokenSigner, stripeAPI corebilling.Stripe, billingSyncer *corebilling.Syncer) *gin.Engine {
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// Gin trusts every proxy's X-Forwarded-For by default, which would let a
	// caller spoof a new IP per request past the public report rate limit.
	if err := router.SetTrustedProxies(cfg.TrustedProxyList()); err != nil {
		slog.Error("invalid TRUSTED_PROXIES; trusting no proxies", "error", err)
		_ = router.SetTrustedProxies(nil)
	}
	router.Use(middleware.RequestID())
	router.Use(middleware.Recovery(cfg))
	router.Use(middleware.SecurityHeaders(cfg))
	router.Use(middleware.LimitBody(maxRequestBody))
	router.Use(cors.New(corsConfig(cfg)))
	// Generous ceiling for any one IP; the webhooks are exempt (signed, bursty).
	router.Use(middleware.RateLimitIP(rdb, "global", 3000, time.Minute, "/api/webhooks"))

	// Every authenticated group shares one verifier (one JWK cache) and the
	// same limits: a general per-user ceiling plus tighter caps on the routes
	// that spend money or send mail.
	authed := []gin.HandlerFunc{
		middleware.RequireAuth(systemDB, cfg),
		middleware.RateLimitUser(rdb, "api", 600, time.Minute),
		middleware.RateLimitRoutes(rdb, expensiveRoutes),
	}
	// The endpoints that need no session get a much lower per-IP ceiling.
	publicLimit := middleware.RateLimitIP(rdb, "public", 120, time.Minute)

	apiV1 := router.Group("/api/v1")
	apiV1.Use(publicLimit)
	v1.NewHealthHandler(db, rdb, pc).Register(apiV1)

	apiAuth := router.Group("/api/v1/auth")
	apiAuth.Use(publicLimit)
	identity.NewDevTokenHandler(cfg).Register(apiAuth)

	// Every route under here requires a verified session; each handler
	// scopes its own queries via tenant.WithTx against the app_user pool.
	settingsHandler := settings.NewHandler(db, encryptionKey, mockKeyPrefix(cfg), emailSender, digestTokens, cfg.AppBaseURL)
	apiSettings := router.Group("/api/v1/settings")
	apiSettings.Use(authed...)
	settingsHandler.Register(apiSettings)

	// Deliberately ungated — a digest email's unsubscribe link has no
	// live Clerk session; Handler.Unsubscribe does its own token-based
	// auth per request instead. Same path prefix as apiSettings above,
	// same "route patterns don't collide" reasoning as
	// apiIntegrationsCallback below.
	apiSettingsPublic := router.Group("/api/v1/settings")
	apiSettingsPublic.Use(publicLimit)
	settingsHandler.RegisterPublic(apiSettingsPublic)

	apiWebhooks := router.Group("/api/webhooks")
	webhooks.NewClerkHandler(systemDB, cfg.ClerkWebhookSecret.Expose()).Register(apiWebhooks)
	webhooks.NewStripeHandler(systemDB, billingSyncer, cfg.StripeWebhookSecret.Expose()).Register(apiWebhooks)

	stateManager := integrations.NewStateManager(rdb, cfg.OAuthStateSecret.Expose())
	intHandler := integrationsapi.NewHandler(db, encryptionKey, registry, stateManager, cfg.FrontendURL)

	apiIntegrations := router.Group("/api/v1/integrations")
	apiIntegrations.Use(authed...)
	intHandler.Register(apiIntegrations)

	// Unauthenticated: the OAuth provider redirects the founder's browser
	// here directly, with no JWT. Same URL prefix as apiIntegrations above;
	// the route patterns don't collide.
	apiIntegrationsCallback := router.Group("/api/v1/integrations")
	apiIntegrationsCallback.Use(publicLimit)
	intHandler.RegisterCallback(apiIntegrationsCallback)

	apiDocuments := router.Group("/api/v1")
	apiDocuments.Use(authed...)
	documents.NewHandler(db, docsStore, docsProcessor, docsSearcher, rdb, encryptionKey).Register(apiDocuments)

	apiAgents := router.Group("/api/v1")
	apiAgents.Use(authed...)
	agents.NewHandler(db, mcpRegistry).Register(apiAgents)

	apiWorkflows := router.Group("/api/v1")
	apiWorkflows.Use(authed...)
	workflowsapi.NewHandler(db, launcher).Register(apiWorkflows)

	apiRuns := router.Group("/api/v1")
	apiRuns.Use(authed...)
	runsapi.NewHandler(db, graphEngine).Register(apiRuns)

	apiAnalytics := router.Group("/api/v1")
	apiAnalytics.Use(authed...)
	analytics.NewHandler(db).Register(apiAnalytics)

	// Dev-only stand-in for the real approve/reject endpoints, registered
	// only when MOCK_LLM_MODE is on.
	if cfg.MockLLMMode {
		runsapi.NewDevResumeHandler(db, launcher).Register(apiRuns)
	}

	approvalsHandler := approvalsapi.NewHandler(db, systemDB, launcher, actionTokens, cfg)

	apiApprovals := router.Group("/api/v1")
	apiApprovals.Use(authed...)
	approvalsHandler.Register(apiApprovals)

	// Deliberately ungated — a push notification's Approve/Reject buttons
	// have no live Clerk session; Handler.resolveActor does its own dual
	// auth (Bearer or ?action_token=) per request instead.
	apiApprovalsActions := router.Group("/api/v1")
	apiApprovalsActions.Use(publicLimit)
	approvalsHandler.RegisterActions(apiApprovalsActions)

	// The zero-value ClientConfig is deliberate: BackendConfig.Key nil falls
	// back to whatever clerk.SetKey(cfg.ClerkSecretKey) already configured
	// at boot — no separate credential wiring needed.
	membershipSyncer := org.NewClerkMembershipSyncer(organizationmembership.NewClient(&clerk.ClientConfig{}))
	invitationLister := org.NewClerkInvitationLister(organizationinvitation.NewClient(&clerk.ClientConfig{}))
	apiOrg := router.Group("/api/v1")
	apiOrg.Use(authed...)
	org.NewHandler(db, membershipSyncer, invitationLister).Register(apiOrg)

	apiBilling := router.Group("/api/v1")
	apiBilling.Use(authed...)
	billing.NewHandler(db, systemDB, stripeAPI, billingSyncer, cfg.FrontendURL).Register(apiBilling)

	apiAuditLogs := router.Group("/api/v1")
	apiAuditLogs.Use(authed...)
	auditlogs.NewHandler(db).Register(apiAuditLogs)

	apiTeams := router.Group("/api/v1")
	apiTeams.Use(authed...)
	teamsapi.NewHandler(db, launcher).Register(apiTeams)

	a2aHandler := a2aapi.NewHandler(db, launcher, taskTokens, cfg.AppBaseURL)

	apiA2A := router.Group("/api/v1")
	apiA2A.Use(authed...)
	a2aHandler.Register(apiA2A)

	apiA2ATasksSend := router.Group("/api/v1")
	apiA2ATasksSend.Use(publicLimit)
	a2aHandler.RegisterTasksSend(apiA2ATasksSend)

	apiTemplates := router.Group("/api/v1")
	apiTemplates.Use(authed...)
	templatesapi.NewHandler(db).Register(apiTemplates)

	apiPractice := router.Group("/api/v1")
	apiPractice.Use(authed...)
	practiceHandler := practiceapi.NewHandler(systemDB, db, practiceapi.NewClerkProvisioner(organization.NewClient(&clerk.ClientConfig{})), mcpRegistry)
	if billingSyncer != nil {
		practiceHandler.SetUsageSyncer(billingSyncer)
	}
	practiceHandler.Register(apiPractice)

	apiIdentity := router.Group("/api/v1")
	apiIdentity.Use(middleware.RequireIdentity(cfg))
	practiceHandler.RegisterIdentityOnly(apiIdentity)

	// 30 views a minute per IP on the public share link; the only
	// unauthenticated read of tenant data in the API.
	reportsHandler := reportsapi.NewHandler(db, systemDB, reportsapi.NewRedisLimiter(rdb, 30, time.Minute))
	apiReports := router.Group("/api/v1")
	apiReports.Use(authed...)
	reportsHandler.Register(apiReports)
	reportsHandler.RegisterPublic(router.Group("/api/public"))

	return router
}

// expensiveRoutes are tighter per-user caps on routes that spend the
// customer's or the platform's money, call Clerk, or send mail. A pattern here
// must match the one the handler registers.
var expensiveRoutes = []middleware.RouteRule{
	{Method: "POST", Path: "/api/v1/settings/api-key", Limit: 10, Window: time.Minute},
	{Method: "POST", Path: "/api/v1/settings/digest/test", Limit: 5, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/settings/push-subscription", Limit: 30, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/integrations/:service/connect", Limit: 20, Window: time.Minute},
	{Method: "POST", Path: "/api/v1/reports", Limit: 30, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/practice/client-workspaces", Limit: 10, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/practice/sops", Limit: 60, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/practice/sops/:id/deploy", Limit: 60, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/agents", Limit: 60, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/workflows", Limit: 60, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/workflows/:id/run", Limit: 120, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/teams", Limit: 30, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/teams/:id/run", Limit: 60, Window: time.Hour},
	{Method: "POST", Path: "/api/v1/templates/:id/install", Limit: 60, Window: time.Hour},
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
// newDocumentsProcessor builds both the ingestion pipeline Processor,
// and the search pipeline (Searcher) off the same
// Cohere client and Pinecone index connection — no reason to pay for a
// second DescribeIndex/connection setup or a second Cohere retry policy.
func newDocumentsProcessor(ctx context.Context, cfg *config.Config, appPool *pgxpool.Pool, store *coredocs.Store, pineconeClient *pinecone.Client) (*coredocs.Processor, *coredocs.Searcher, error) {
	// WithMaxAttempts(7): a large (480+ chunk) document can outlast
	// Cohere's own retrier's default 2 attempts against its per-minute
	// rate limit — 7 gives ~63s of cumulative backoff, enough to cover one
	// full window reset. Reused as-is for query/rerank calls at search
	// time, which are far smaller but benefit from the same resilience.
	cohereClient := coherecli.NewClient(coreoption.WithToken(cfg.CohereAPIKey.Expose()), coreoption.WithMaxAttempts(7))

	idx, err := pineconeClient.DescribeIndex(ctx, cfg.PineconeIndexRAG)
	if err != nil {
		return nil, nil, fmt.Errorf("describe pinecone rag index %q: %w", cfg.PineconeIndexRAG, err)
	}
	idxConn, err := pineconeClient.Index(pinecone.NewIndexConnParams{Host: idx.Host})
	if err != nil {
		return nil, nil, fmt.Errorf("connect to pinecone rag index: %w", err)
	}

	embedder := coredocs.NewCohereEmbedder(cohereClient)
	index := coredocs.NewPineconeIndex(idxConn)
	processor := coredocs.NewProcessor(appPool, store, embedder, index)
	searcher := coredocs.NewSearcher(embedder, index, coredocs.NewCohereReranker(cohereClient))
	return processor, searcher, nil
}

// corsConfig is wide open in development, locked to the app's own origins
// in production. AllowOriginFunc, not AllowOrigins: []string{"*"}, for the
// dev case — the CORS spec forbids combining a wildcard origin with
// AllowCredentials.
func corsConfig(cfg *config.Config) cors.Config {
	c := cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Accept", "X-Action-Token"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	if cfg.IsProduction() {
		c.AllowOrigins = []string{cfg.AppBaseURL, cfg.FrontendURL, "https://founderstack.ai"}
	} else {
		c.AllowOriginFunc = func(origin string) bool { return true }
	}
	return c
}

// mockKeyPrefix is the BYOK validation short-circuit prefix, disabled in
// production so a pasted "mock-test-key-..." can never be stored as valid.
func mockKeyPrefix(cfg *config.Config) string {
	if cfg.IsProduction() {
		return ""
	}
	return cfg.APIKeyMockPrefix
}
