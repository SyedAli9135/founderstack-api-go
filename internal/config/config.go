package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"

	"github.com/founderstack/api/internal/pkg/secret"
)

// Config holds all runtime settings for the API process. Fields are
// populated once at startup by Load and never mutated afterward, so a
// *Config can be shared freely across goroutines.
type Config struct {
	AppEnv      string `mapstructure:"APP_ENV"`
	AppBaseURL  string `mapstructure:"APP_BASE_URL"`
	FrontendURL string `mapstructure:"FRONTEND_URL"`

	// DatabaseURL connects as the postgres superuser (migrate CLI, RLS
	// exempt). AppDatabaseURL is the restricted, RLS-enforced app_user role
	// the API server itself connects with.
	DatabaseURL       string `mapstructure:"DATABASE_URL"`
	AppDatabaseURL    string `mapstructure:"APP_DATABASE_URL"`
	SystemDatabaseURL string `mapstructure:"SYSTEM_DATABASE_URL"`
	DatabasePoolSize  int    `mapstructure:"DATABASE_POOL_SIZE"`

	// Auth (Clerk)
	ClerkSecretKey      secret.Value `mapstructure:"CLERK_SECRET_KEY"`
	ClerkPublishableKey string       `mapstructure:"CLERK_PUBLISHABLE_KEY"`
	ClerkWebhookSecret  secret.Value `mapstructure:"CLERK_WEBHOOK_SECRET"`

	// LocalStack (local dev S3 mock)
	LocalstackAuthToken secret.Value `mapstructure:"LOCALSTACK_AUTH_TOKEN"`

	// OAuth / API-key client credentials
	SlackClientID        string       `mapstructure:"SLACK_CLIENT_ID"`
	SlackClientSecret    secret.Value `mapstructure:"SLACK_CLIENT_SECRET"`
	DiscordClientID      string       `mapstructure:"DISCORD_CLIENT_ID"`
	DiscordClientSecret  secret.Value `mapstructure:"DISCORD_CLIENT_SECRET"`
	NotionClientID       string       `mapstructure:"NOTION_CLIENT_ID"`
	NotionClientSecret   secret.Value `mapstructure:"NOTION_CLIENT_SECRET"`
	GoogleClientID       string       `mapstructure:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret   secret.Value `mapstructure:"GOOGLE_CLIENT_SECRET"`
	LinkedInClientID     string       `mapstructure:"LINKEDIN_CLIENT_ID"`
	LinkedInClientSecret secret.Value `mapstructure:"LINKEDIN_CLIENT_SECRET"`

	// LLM / AI
	PineconeAPIKey     secret.Value `mapstructure:"PINECONE_API_KEY"`
	PineconeIndexRAG   string       `mapstructure:"PINECONE_INDEX_RAG"`
	PineconeIndexTools string       `mapstructure:"PINECONE_INDEX_TOOLS"`

	UpstashRedisURL   string       `mapstructure:"UPSTASH_REDIS_URL"`
	UpstashRedisToken secret.Value `mapstructure:"UPSTASH_REDIS_TOKEN"`

	NangoSecretKey secret.Value `mapstructure:"NANGO_SECRET_KEY"`
	CohereAPIKey   secret.Value `mapstructure:"COHERE_API_KEY"`
	AWSRegion      string       `mapstructure:"AWS_REGION"`

	// Document storage (S3 / LocalStack)
	S3BucketDocuments  string       `mapstructure:"S3_BUCKET_DOCUMENTS"`
	AWSAccessKeyID     string       `mapstructure:"AWS_ACCESS_KEY_ID"`
	AWSSecretAccessKey secret.Value `mapstructure:"AWS_SECRET_ACCESS_KEY"`
	// AWSS3EndpointURL points at LocalStack (http://localhost:4566) in local
	// dev; leave empty to hit real AWS.
	AWSS3EndpointURL string `mapstructure:"AWS_S3_ENDPOINT_URL"`

	// Security
	EncryptionKey    secret.Value `mapstructure:"ENCRYPTION_KEY"`
	OAuthStateSecret secret.Value `mapstructure:"OAUTH_STATE_SECRET"`
	// APIKeyMockPrefix short-circuits BYOK key validation to success without
	// a network call, for local dev/tests — one prefix shared across all 5
	// llm.Catalog providers rather than one env var each.
	APIKeyMockPrefix string `mapstructure:"API_KEY_MOCK_PREFIX"`

	// DevTokenSecret signs local test JWTs for POST /api/v1/auth/dev-token.
	// Deliberately optional (not in requiredFields) — unset disables the
	// dev token fallback path entirely.
	DevTokenSecret secret.Value `mapstructure:"DEV_TOKEN_SECRET"`

	// MockLLMMode swaps every workflow run's real BYOK ChatClient for a
	// scripted llm.MockScenarioResolver and enables a dev-only
	// approval-resume debug route, for exercising the agent execution
	// engine with zero live LLM calls. main.go additionally asserts
	// !cfg.IsProduction() before honoring this.
	MockLLMMode bool `mapstructure:"MOCK_LLM_MODE"`

	// Approval-gate notifications — all 6 deliberately optional, same
	// "degrades to a logged no-op" convention as other third-party
	// secrets. BrevoAPIKey, not SendGrid: Brevo has a free-forever tier.
	// WebPushVAPIDPublicKey isn't a secret.Value — it ships to the frontend
	// as-is.
	BrevoAPIKey            secret.Value `mapstructure:"BREVO_API_KEY"`
	BrevoFromEmail         string       `mapstructure:"BREVO_FROM_EMAIL"`
	WebPushVAPIDPublicKey  string       `mapstructure:"WEBPUSH_VAPID_PUBLIC_KEY"`
	WebPushVAPIDPrivateKey secret.Value `mapstructure:"WEBPUSH_VAPID_PRIVATE_KEY"`
	WebPushVAPIDSubject    string       `mapstructure:"WEBPUSH_VAPID_SUBJECT"`
	// PushActionTokenSecret signs the single-purpose action tokens in a push
	// notification's payload so its Approve/Reject buttons work without
	// opening the app — dedicated, not reused from OAuthStateSecret, to
	// contain blast radius between the two signing use cases.
	PushActionTokenSecret secret.Value `mapstructure:"PUSH_ACTION_TOKEN_SECRET"`
}

// requiredFields lists the mapstructure keys that must resolve to a
// non-empty value. Kept in one place so Load's validation and any future
// "print what's missing" tooling stay in sync.
var requiredFields = []struct {
	key   string
	value func(*Config) string
}{
	{"DATABASE_URL", func(c *Config) string { return c.DatabaseURL }},
	{"APP_DATABASE_URL", func(c *Config) string { return c.AppDatabaseURL }},
	{"SYSTEM_DATABASE_URL", func(c *Config) string { return c.SystemDatabaseURL }},
	{"CLERK_SECRET_KEY", func(c *Config) string { return c.ClerkSecretKey.Expose() }},
	{"CLERK_PUBLISHABLE_KEY", func(c *Config) string { return c.ClerkPublishableKey }},
	{"CLERK_WEBHOOK_SECRET", func(c *Config) string { return c.ClerkWebhookSecret.Expose() }},
	{"LOCALSTACK_AUTH_TOKEN", func(c *Config) string { return c.LocalstackAuthToken.Expose() }},
	{"PINECONE_API_KEY", func(c *Config) string { return c.PineconeAPIKey.Expose() }},
	{"ENCRYPTION_KEY", func(c *Config) string { return c.EncryptionKey.Expose() }},
	{"OAUTH_STATE_SECRET", func(c *Config) string { return c.OAuthStateSecret.Expose() }},
}

// IsProduction reports whether the process is running with APP_ENV=production.
func (c *Config) IsProduction() bool {
	return c.AppEnv == "production"
}

// Load reads configuration from process environment variables, falling
// back to a ".env" file in the working directory when present (development
// convenience only — nothing in production should rely on a .env file
// existing). It returns an error naming every required-but-unset variable
// rather than failing on the first one, so a misconfigured environment can
// be fixed in a single pass.
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: reading .env: %w", err)
	}

	v := viper.New()
	v.AutomaticEnv()

	defaults := map[string]any{
		"APP_ENV":                "development",
		"APP_BASE_URL":           "http://localhost:8000",
		"FRONTEND_URL":           "http://localhost:3000",
		"DATABASE_URL":           "",
		"APP_DATABASE_URL":       "",
		"SYSTEM_DATABASE_URL":    "",
		"DATABASE_POOL_SIZE":     20,
		"CLERK_SECRET_KEY":       "",
		"CLERK_PUBLISHABLE_KEY":  "",
		"CLERK_WEBHOOK_SECRET":   "",
		"LOCALSTACK_AUTH_TOKEN":  "",
		"SLACK_CLIENT_ID":        "",
		"SLACK_CLIENT_SECRET":    "",
		"DISCORD_CLIENT_ID":      "",
		"DISCORD_CLIENT_SECRET":  "",
		"NOTION_CLIENT_ID":       "",
		"NOTION_CLIENT_SECRET":   "",
		"GOOGLE_CLIENT_ID":       "",
		"GOOGLE_CLIENT_SECRET":   "",
		"LINKEDIN_CLIENT_ID":     "",
		"LINKEDIN_CLIENT_SECRET": "",
		"PINECONE_API_KEY":       "",
		"PINECONE_INDEX_RAG":     "founderstack-rag",
		"PINECONE_INDEX_TOOLS":   "founderstack-tools",
		"UPSTASH_REDIS_URL":      "",
		"UPSTASH_REDIS_TOKEN":    "",
		"NANGO_SECRET_KEY":       "",
		"COHERE_API_KEY":         "",
		"AWS_REGION":             "us-east-1",
		"S3_BUCKET_DOCUMENTS":    "founderstack-documents",
		"AWS_ACCESS_KEY_ID":      "test",
		"AWS_SECRET_ACCESS_KEY":  "test",
		"AWS_S3_ENDPOINT_URL":    "",
		"ENCRYPTION_KEY":         "",
		"OAUTH_STATE_SECRET":     "",
		"API_KEY_MOCK_PREFIX":    "mock-test-key-",
		"DEV_TOKEN_SECRET":       "",
		"MOCK_LLM_MODE":          false,

		"BREVO_API_KEY":             "",
		"BREVO_FROM_EMAIL":          "",
		"WEBPUSH_VAPID_PUBLIC_KEY":  "",
		"WEBPUSH_VAPID_PRIVATE_KEY": "",
		"WEBPUSH_VAPID_SUBJECT":     "",
		"PUSH_ACTION_TOKEN_SECRET":  "",
	}
	for key, def := range defaults {
		v.SetDefault(key, def)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	var missing []string
	for _, f := range requiredFields {
		if strings.TrimSpace(f.value(&cfg)) == "" {
			missing = append(missing, f.key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required environment variable(s): %s", strings.Join(missing, ", "))
	}

	return &cfg, nil
}
