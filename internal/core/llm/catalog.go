package llm

// ProviderID identifies a BYOK-supported LLM provider. A plain string, not
// a Postgres enum, matching api_key_registry/organizations.llm_provider's
// varchar(50) columns.
type ProviderID string

const (
	ProviderAnthropic ProviderID = "anthropic"
	ProviderOpenAI    ProviderID = "openai"
	ProviderGemini    ProviderID = "gemini"
	ProviderQwen      ProviderID = "qwen"
	ProviderDeepSeek  ProviderID = "deepseek"
)

// Meta is one catalog entry: display name plus the real key format's
// prefix, used for the cheap pre-network format check and error wording.
type Meta struct {
	Name      string
	KeyPrefix string
}

// defaultModels is what an agent created without a model runs on: a current,
// mid-priced chat model per provider. Model IDs are the provider's own, so a
// default for one provider is invalid on another — it must follow the org's
// active provider.
var defaultModels = map[ProviderID]string{
	ProviderAnthropic: "claude-sonnet-5-5",
	ProviderOpenAI:    "gpt-4o",
	ProviderGemini:    "gemini-2.0-flash",
	ProviderQwen:      "qwen-plus",
	ProviderDeepSeek:  "deepseek-chat",
}

// DefaultModel returns the default chat model for a provider, falling back to
// Anthropic's for an unknown or empty one.
func DefaultModel(p ProviderID) string {
	if m, ok := defaultModels[p]; ok {
		return m
	}
	return defaultModels[ProviderAnthropic]
}

// Catalog is the static list of every LLM provider BYOK supports. Adding
// provider #6 is one line here plus one verify func in verify.go.
var Catalog = map[ProviderID]Meta{
	ProviderAnthropic: {Name: "Anthropic", KeyPrefix: "sk-ant-"},
	ProviderOpenAI:    {Name: "OpenAI", KeyPrefix: "sk-"},
	ProviderGemini:    {Name: "Google Gemini", KeyPrefix: "AIza"},
	ProviderQwen:      {Name: "Qwen", KeyPrefix: "sk-"},
	ProviderDeepSeek:  {Name: "DeepSeek", KeyPrefix: "sk-"},
}
