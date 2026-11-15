package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveJSON(t *testing.T, status int, body string, check func(*http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ids(ms []ModelInfo) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestListModels_GeminiKeepsOnlyAgentCapableGenerateContent(t *testing.T) {
	var gotKey string
	srv := serveJSON(t, 200, `{"models":[
	 {"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash","supportedGenerationMethods":["generateContent"]},
	 {"name":"models/gemini-2.5-flash-preview-tts","supportedGenerationMethods":["generateContent"]},
	 {"name":"models/gemini-embedding-001","supportedGenerationMethods":["embedContent"]},
	 {"name":"models/gemma-4-31b-it","supportedGenerationMethods":["generateContent"]},
	 {"name":"models/gemini-flash-latest","supportedGenerationMethods":["generateContent"]}]}`,
		func(r *http.Request) { gotKey = r.Header.Get("x-goog-api-key") })
	defer swapGeminiURL(srv.URL)()

	got, err := ListModels(context.Background(), ProviderGemini, "AQ.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gemini-2.5-flash", "gemini-flash-latest"}; len(got) != 2 || got[0].ID != want[0] || got[1].ID != want[1] {
		t.Fatalf("got %v, want %v", ids(got), want)
	}
	if gotKey != "AQ.test" {
		t.Fatalf("key header = %q", gotKey)
	}
}

func TestListModels_AnthropicSendsHeadersAndNames(t *testing.T) {
	var key, ver string
	srv := serveJSON(t, 200, `{"data":[{"id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5"},{"id":"claude-haiku-5-5","display_name":""}]}`,
		func(r *http.Request) { key, ver = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version") })
	old := anthropicModelsURL
	anthropicModelsURL = srv.URL
	defer func() { anthropicModelsURL = old }()

	got, err := ListModels(context.Background(), ProviderAnthropic, "sk-ant-x")
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-ant-x" || ver == "" {
		t.Fatalf("headers key=%q version=%q", key, ver)
	}
	if got[0].ID != "claude-haiku-5-5" || got[0].DisplayName != "claude-haiku-5-5" || got[1].DisplayName != "Claude Sonnet 5.5" {
		t.Fatalf("got %+v", got)
	}
}

func TestListModels_OpenAIFiltersNonChat(t *testing.T) {
	srv := serveJSON(t, 200, `{"data":[{"id":"gpt-4o"},{"id":"text-embedding-3-small"},{"id":"whisper-1"},{"id":"o3-mini"},{"id":"gpt-4o-realtime-preview"},{"id":"dall-e-3"},{"id":"gpt-4o-mini-tts"},{"id":"omni-moderation-latest"}]}`, nil)
	old := openAIModelsURL
	openAIModelsURL = srv.URL
	defer func() { openAIModelsURL = old }()

	got, err := ListModels(context.Background(), ProviderOpenAI, "sk-x")
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); len(g) != 2 || g[0] != "gpt-4o" || g[1] != "o3-mini" {
		t.Fatalf("got %v", g)
	}
}

func TestListModels_QwenAndDeepSeek(t *testing.T) {
	q := serveJSON(t, 200, `{"data":[{"id":"qwen-plus"},{"id":"text-embedding-v3"},{"id":"qwen-vl-max"},{"id":"qwq-plus"}]}`, nil)
	d := serveJSON(t, 200, `{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`, nil)
	oq, od := qwenModelsURL, deepSeekModelsURL
	qwenModelsURL, deepSeekModelsURL = q.URL, d.URL
	defer func() { qwenModelsURL, deepSeekModelsURL = oq, od }()

	gq, err := ListModels(context.Background(), ProviderQwen, "sk-x")
	if err != nil || len(gq) != 2 || gq[0].ID != "qwen-plus" || gq[1].ID != "qwq-plus" {
		t.Fatalf("qwen got %v err %v", ids(gq), err)
	}
	gd, err := ListModels(context.Background(), ProviderDeepSeek, "sk-x")
	if err != nil || len(gd) != 2 {
		t.Fatalf("deepseek got %v err %v", ids(gd), err)
	}
}

func TestListModels_ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{401, ErrKeyRejected}, {403, ErrKeyRejected}, {400, ErrKeyRejected}, {500, ErrValidationUnavailable}, {429, ErrValidationUnavailable}} {
		srv := serveJSON(t, tc.status, `{}`, nil)
		old := deepSeekModelsURL
		deepSeekModelsURL = srv.URL
		_, err := ListModels(context.Background(), ProviderDeepSeek, "sk-x")
		deepSeekModelsURL = old
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
	if _, err := ListModels(context.Background(), "nope", "k"); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("unknown provider err = %v", err)
	}
}

func TestPreferredModel(t *testing.T) {
	m := func(s ...string) []ModelInfo {
		var o []ModelInfo
		for _, x := range s {
			o = append(o, ModelInfo{ID: x})
		}
		return o
	}
	cases := []struct {
		p    ProviderID
		in   []ModelInfo
		want string
	}{
		{ProviderGemini, m("gemini-2.5-flash", "gemini-2.5-pro"), "gemini-2.5-flash"},                     // static default still offered
		{ProviderGemini, m("gemini-3-pro", "gemini-3-flash-preview", "gemini-3-flash"), "gemini-3-flash"}, // family match, skips preview
		{ProviderGemini, m("gemini-x-ultra"), "gemini-x-ultra"},                                           // nothing matches: first
		{ProviderAnthropic, m("claude-haiku-9", "claude-sonnet-9"), "claude-sonnet-9"},
		{ProviderOpenAI, nil, ""},
	}
	for _, c := range cases {
		if got := PreferredModel(c.p, c.in); got != c.want {
			t.Errorf("%s %v: got %q want %q", c.p, ids(c.in), got, c.want)
		}
	}
}

func TestProbeModel_ReportsProviderRefusalAndSuccess(t *testing.T) {
	refuse := serveJSON(t, 404, `{"error":{"message":"This model is no longer available to new users."}}`, nil)
	defer swapGeminiGenerateURL(refuse.URL)()
	_, err := ProbeModel(context.Background(), ProviderGemini, "k", "gemini-old")
	if err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("err = %v, want provider's own message", err)
	}

	ok := serveJSON(t, 200, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`, nil)
	defer swapGeminiGenerateURL(ok.URL)()
	if _, err := ProbeModel(context.Background(), ProviderGemini, "k", "gemini-good"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func swapGeminiGenerateURL(u string) func() {
	old := geminiGenerateContentBaseURL
	geminiGenerateContentBaseURL = u
	return func() { geminiGenerateContentBaseURL = old }
}
