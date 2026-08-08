// Command api runs the FounderStack API HTTP server.
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

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	v1 "github.com/founderstack/api/internal/api/v1"
	"github.com/founderstack/api/internal/config"
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

	// Cancelled on SIGINT/SIGTERM; everything below shuts down off this.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer dbPool.Close()

	redisClient, err := newRedisClient(cfg)
	if err != nil {
		return fmt.Errorf("configure redis client: %w", err)
	}
	defer redisClient.Close()

	pineconeClient, err := newPineconeClient(cfg)
	if err != nil {
		return fmt.Errorf("configure pinecone client: %w", err)
	}

	router := newRouter(cfg, dbPool, redisClient, pineconeClient)

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

func newRouter(cfg *config.Config, db *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client) *gin.Engine {
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(cors.New(corsConfig(cfg)))

	apiV1 := router.Group("/api/v1")
	v1.NewHealthHandler(db, rdb, pc).Register(apiV1)

	return router
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
