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

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/identity"
	integrationsapi "github.com/founderstack/api/internal/api/integrations"
	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/settings"
	v1 "github.com/founderstack/api/internal/api/v1"
	"github.com/founderstack/api/internal/api/webhooks"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/core/integrations/providers"
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
	router := newRouter(cfg, dbPool, systemPool, redisClient, pineconeClient, encryptionKey, integrationsRegistry)

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

func newRouter(cfg *config.Config, db, systemDB *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client, encryptionKey []byte, registry *integrations.Registry) *gin.Engine {
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
	settings.NewHandler(db, encryptionKey, cfg.AnthropicAPIKeyMockPrefix).Register(apiSettings)

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
