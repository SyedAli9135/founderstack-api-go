package llm

import "testing"

func TestDefaultModel_EveryCatalogProviderHasOneAndUnknownFallsBack(t *testing.T) {
	for id := range Catalog {
		if DefaultModel(id) == "" {
			t.Errorf("provider %q has no default model", id)
		}
	}
	if DefaultModel("") != DefaultModel(ProviderAnthropic) || DefaultModel("nope") != DefaultModel(ProviderAnthropic) {
		t.Error("an empty or unknown provider should fall back to Anthropic's default")
	}
	if DefaultModel(ProviderOpenAI) == DefaultModel(ProviderAnthropic) {
		t.Error("providers must not share a default model ID — IDs are per-provider")
	}
}
