package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func testContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, rec
}

func TestOK(t *testing.T) {
	c, rec := testContext(t)
	OK(c, http.StatusCreated, "created", map[string]string{"id": "abc"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	var got Success
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Status != "success" || got.Message != "created" {
		t.Fatalf("got %+v, want status=success message=created", got)
	}
}

func TestFail_IncludesRequestID(t *testing.T) {
	c, rec := testContext(t)
	SetRequestID(c, "req-123")
	Fail(c, http.StatusBadRequest, "BAD_INPUT", "nope")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var got Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Status != "error" || got.Error.Code != "BAD_INPUT" || got.Error.RequestID != "req-123" {
		t.Fatalf("got %+v, want status=error code=BAD_INPUT request_id=req-123", got)
	}
}

func TestFail_NoRequestIDSetYieldsEmptyString(t *testing.T) {
	c, rec := testContext(t)
	Fail(c, http.StatusInternalServerError, "OOPS", "broke")

	var got Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Error.RequestID != "" {
		t.Fatalf("request_id = %q, want empty when SetRequestID was never called", got.Error.RequestID)
	}
}

func TestFailWithDetail_IncludesDetail(t *testing.T) {
	c, rec := testContext(t)
	detail := []string{"field a is required", "field b must be positive"}
	FailWithDetail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "bad input", detail)

	var got struct {
		Error struct {
			Detail []string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Error.Detail) != 2 || got.Error.Detail[0] != detail[0] {
		t.Fatalf("detail = %v, want %v", got.Error.Detail, detail)
	}
}

func TestRequestID_RoundTrips(t *testing.T) {
	c, _ := testContext(t)
	if got := RequestID(c); got != "" {
		t.Fatalf("RequestID before SetRequestID = %q, want empty", got)
	}
	SetRequestID(c, "abc-def")
	if got := RequestID(c); got != "abc-def" {
		t.Fatalf("RequestID after SetRequestID = %q, want %q", got, "abc-def")
	}
}

func TestNewID(t *testing.T) {
	a := NewID()
	b := NewID()
	if a == b {
		t.Fatal("NewID() returned the same value twice in a row")
	}
	if len(a) != 32 {
		t.Fatalf("len(NewID()) = %d, want 32 (16 bytes hex-encoded)", len(a))
	}
}
