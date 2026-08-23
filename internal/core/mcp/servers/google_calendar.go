package servers

import (
	"context"
	"fmt"
	"net/url"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// calendarAPIBase is a var so google_calendar_test.go can point it at a
// fake server.
var calendarAPIBase = "https://www.googleapis.com/calendar/v3"

// NewGoogleCalendarServer builds the Google Calendar MCP tool server —
// list_events and create_event, both against the founder's primary
// calendar. workflow 4 granted the `calendar.events` scope (see
// internal/core/integrations/providers/google_calendar.go), which is
// event-level read/write on calendars the founder already has access
// to — no calendar-creation/sharing capability, which these 2 tools
// don't need anyway.
func NewGoogleCalendarServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "google_calendar", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "list_events",
		Description: "List upcoming events on the founder's primary Google Calendar.",
	}, calendarListEvents)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "create_event",
		Description: "Create an event on the founder's primary Google Calendar.",
	}, calendarCreateEvent)

	return server
}

type calendarListEventsInput struct {
	TimeMin string `json:"time_min,omitempty" jsonschema:"RFC3339 start of the search window (default: now)"`
	TimeMax string `json:"time_max,omitempty" jsonschema:"RFC3339 end of the search window (default: unbounded)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum events to return (default 20, max 100)"`
}

type calendarEventSummary struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	HTMLLink string `json:"html_link,omitempty"`
}

type calendarListEventsOutput struct {
	Events []calendarEventSummary `json:"events"`
}

type calendarEventsListResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
		Start   struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"end"`
		HTMLLink string `json:"htmlLink"`
	} `json:"items"`
}

func calendarListEvents(ctx context.Context, req *gomcp.CallToolRequest, in calendarListEventsInput) (*gomcp.CallToolResult, calendarListEventsOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, calendarListEventsOutput{}, fmt.Errorf("google_calendar: no token in request metadata")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	timeMin := in.TimeMin
	if timeMin == "" {
		timeMin = time.Now().UTC().Format(time.RFC3339)
	}

	// RFC3339 timestamps can carry a "+HH:MM" offset — "+" means space in
	// a query string unless escaped, so this can't be a bare string
	// concatenation.
	endpoint := fmt.Sprintf("%s/calendars/primary/events?singleEvents=true&orderBy=startTime&maxResults=%d&timeMin=%s",
		calendarAPIBase, limit, url.QueryEscape(timeMin))
	if in.TimeMax != "" {
		endpoint += "&timeMax=" + url.QueryEscape(in.TimeMax)
	}

	var resp calendarEventsListResponse
	if err := doJSON(ctx, "GET", endpoint, token, nil, &resp); err != nil {
		return nil, calendarListEventsOutput{}, fmt.Errorf("google_calendar: list events: %w", err)
	}

	out := calendarListEventsOutput{Events: make([]calendarEventSummary, 0, len(resp.Items))}
	for _, item := range resp.Items {
		start := item.Start.DateTime
		if start == "" {
			start = item.Start.Date
		}
		end := item.End.DateTime
		if end == "" {
			end = item.End.Date
		}
		out.Events = append(out.Events, calendarEventSummary{
			ID: item.ID, Summary: item.Summary, Start: start, End: end, HTMLLink: item.HTMLLink,
		})
	}
	return nil, out, nil
}

type calendarCreateEventInput struct {
	Summary     string `json:"summary" jsonschema:"Event title"`
	Description string `json:"description,omitempty" jsonschema:"Event description"`
	StartTime   string `json:"start_time" jsonschema:"RFC3339 start datetime, e.g. 2026-09-01T10:00:00Z"`
	EndTime     string `json:"end_time" jsonschema:"RFC3339 end datetime"`
	TimeZone    string `json:"time_zone,omitempty" jsonschema:"IANA timezone, e.g. America/New_York (default UTC)"`
}

type calendarCreateEventOutput struct {
	EventID  string `json:"event_id"`
	HTMLLink string `json:"html_link,omitempty"`
}

type calendarCreatedEventResponse struct {
	ID       string `json:"id"`
	HTMLLink string `json:"htmlLink"`
}

func calendarCreateEvent(ctx context.Context, req *gomcp.CallToolRequest, in calendarCreateEventInput) (*gomcp.CallToolResult, calendarCreateEventOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, calendarCreateEventOutput{}, fmt.Errorf("google_calendar: no token in request metadata")
	}
	if in.Summary == "" || in.StartTime == "" || in.EndTime == "" {
		return nil, calendarCreateEventOutput{}, fmt.Errorf("google_calendar: summary, start_time, and end_time are required")
	}
	tz := in.TimeZone
	if tz == "" {
		tz = "UTC"
	}

	payload := map[string]any{
		"summary": in.Summary,
		"start":   map[string]string{"dateTime": in.StartTime, "timeZone": tz},
		"end":     map[string]string{"dateTime": in.EndTime, "timeZone": tz},
	}
	if in.Description != "" {
		payload["description"] = in.Description
	}

	var created calendarCreatedEventResponse
	if err := doJSON(ctx, "POST", calendarAPIBase+"/calendars/primary/events", token, payload, &created); err != nil {
		return nil, calendarCreateEventOutput{}, fmt.Errorf("google_calendar: create event: %w", err)
	}

	return nil, calendarCreateEventOutput{EventID: created.ID, HTMLLink: created.HTMLLink}, nil
}
