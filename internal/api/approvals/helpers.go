package approvals

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/api/response"
)

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func parseApprovalID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid approval id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

func formatTimestamptz(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.Format(rfc3339)
	return &s
}

// nowUTC is a var (not a bare time.Now() call) purely so a future test
// could override it — not exercised yet, but costs nothing and matches
// this codebase's general "make time comparisons overridable" caution
// around expiry logic.
var nowUTC = func() time.Time { return time.Now().UTC() }
