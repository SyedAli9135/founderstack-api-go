package authctx

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFromContext_NotSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	_, ok := FromContext(c)
	if ok {
		t.Fatal("FromContext() ok = true before Set was ever called, want false")
	}
}

func TestSetFromContext_RoundTrips(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	var id pgtype.UUID
	if err := id.Scan("11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	want := User{ID: id, OrgID: id, Role: "admin", OrgName: "Acme", OrgSlug: "acme"}

	Set(c, want)
	got, ok := FromContext(c)
	if !ok {
		t.Fatal("FromContext() ok = false after Set, want true")
	}
	if got != want {
		t.Fatalf("FromContext() = %+v, want %+v", got, want)
	}
}
