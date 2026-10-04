package errreport

import "testing"

func TestDisabledWithoutDSN(t *testing.T) {
	flush, err := Init("", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	flush()
	Panic("job", "boom", map[string]string{"k": "v"})
	Message("http", "5xx", nil)
}

func TestInvalidDSNFailsInit(t *testing.T) {
	if _, err := Init("not-a-dsn", "test", ""); err == nil {
		t.Fatal("a malformed SENTRY_DSN must fail boot loudly, not silently disable reporting")
	}
}
