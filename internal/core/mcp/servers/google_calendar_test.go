package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func connectGoogleCalendarServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewGoogleCalendarServer()
	serverTransport, clientTransport := gomcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func swapCalendarAPIBase(url string) func() {
	original := calendarAPIBase
	calendarAPIBase = url
	return func() { calendarAPIBase = original }
}

func TestGoogleCalendar_ListEvents(t *testing.T) {
	var gotPath, gotTimeMin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTimeMin = r.URL.Query().Get("timeMin")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"e1","summary":"Standup","start":{"dateTime":"2026-09-01T10:00:00Z"},"end":{"dateTime":"2026-09-01T10:30:00Z"},"htmlLink":"https://calendar.google.com/e1"}]}`))
	}))
	defer srv.Close()
	defer swapCalendarAPIBase(srv.URL)()

	session := connectGoogleCalendarServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "list_events",
		Arguments: map[string]any{"time_min": "2026-09-01T00:00:00+05:00"},
		Meta:      mcp.WithToken("calendar-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotPath != "/calendars/primary/events" {
		t.Errorf("path = %q, want /calendars/primary/events", gotPath)
	}
	// The "+" in the offset must have survived URL-encoding, not been
	// silently turned into a space by the query string.
	if gotTimeMin != "2026-09-01T00:00:00+05:00" {
		t.Errorf("timeMin received by server = %q, want 2026-09-01T00:00:00+05:00 (the + must survive encoding)", gotTimeMin)
	}

	var out calendarListEventsOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || out.Events[0].Summary != "Standup" {
		t.Fatalf("events = %+v, want one Standup", out.Events)
	}
}

func TestGoogleCalendar_CreateEvent(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"newevent1","htmlLink":"https://calendar.google.com/newevent1"}`))
	}))
	defer srv.Close()
	defer swapCalendarAPIBase(srv.URL)()

	session := connectGoogleCalendarServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "create_event",
		Arguments: map[string]any{
			"summary":    "Investor call",
			"start_time": "2026-09-01T10:00:00Z",
			"end_time":   "2026-09-01T10:30:00Z",
		},
		Meta: mcp.WithToken("calendar-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotBody["summary"] != "Investor call" {
		t.Errorf("body = %+v, want summary=Investor call", gotBody)
	}

	var out calendarCreateEventOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.EventID != "newevent1" {
		t.Fatalf("event_id = %q, want newevent1", out.EventID)
	}
}

func TestGoogleCalendar_CreateEvent_MissingFieldsIsToolError(t *testing.T) {
	session := connectGoogleCalendarServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "create_event",
		Arguments: map[string]any{"summary": "No times set"},
		Meta:      mcp.WithToken("calendar-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for missing start_time/end_time")
	}
}
