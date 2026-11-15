package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsListModels(t *testing.T) {
	systemPool, appPool := testSystemPool(t), testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	clerkUserID := testOrgAndUser(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, encKey)

	get := func(provider string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/providers/"+provider+"/models", nil))
		return rec
	}

	t.Run("unknown provider is 400", func(t *testing.T) {
		if rec := get("nope"); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no key for the provider is 400 NO_API_KEY", func(t *testing.T) {
		rec := get("gemini")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "NO_API_KEY") {
			t.Fatalf("body = %s, want NO_API_KEY", rec.Body.String())
		}
	})

	t.Run("a mock key gets a canned list and a recommendation", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key",
			map[string]string{"api_key": "mock-test-key-abcdefghijklmnop", "provider": "gemini"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("submit status = %d; %s", rec.Code, rec.Body.String())
		}

		rec = get("gemini")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data struct {
				Provider    string `json:"provider"`
				Recommended string `json:"recommended"`
				Models      []struct {
					ID string `json:"id"`
				} `json:"models"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data.Provider != "gemini" || len(got.Data.Models) != 1 || got.Data.Recommended != got.Data.Models[0].ID {
			t.Fatalf("got %+v", got.Data)
		}
	})
}
