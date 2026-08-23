package llm

// ProviderID identifies one of the LLM providers BYOK can validate and
// store a key for. A plain string, not a Postgres enum — api_key_registry
// (and organizations.llm_provider)'s columns are varchar(50), matching the
// same "caller-supplied string, not a DB enum" choice already made for
// internal/core/integrations' service_name.
type ProviderID string

const (
	ProviderAnthropic ProviderID = "anthropic"
	ProviderOpenAI    ProviderID = "openai"
	ProviderGemini    ProviderID = "gemini"
	ProviderQwen      ProviderID = "qwen"
	ProviderDeepSeek  ProviderID = "deepseek"
)

// Meta describes one catalog entry: the founder-facing display name and
// the real key format's prefix (used both for the cheap pre-network-call
// format check and to word the invalid-key error message). Mirrors
// internal/core/integrations/catalog.go's Meta/Catalog shape.
type Meta struct {
	Name      string
	KeyPrefix string
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
