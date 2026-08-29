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

// connectStripeServer wires a real MCP client/server pair over
// mcp.NewInMemoryTransports() — the same connection mechanism
// internal/core/mcp.NewRegistry uses in production — so these tests
// exercise AddTool's real schema validation and dispatch, not just the
// underlying HTTP helper functions directly.
func connectStripeServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewStripeServer()
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

func TestStripe_ListSubscriptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/subscriptions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "sk_test_whatever" || pass != "" {
			t.Errorf("unexpected auth: user=%q pass=%q ok=%v", user, pass, ok)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"sub_1","status":"active","customer":"cus_1"}],"has_more":false}`))
	}))
	defer srv.Close()
	restoreAPIBase := swapStripeAPIBase(srv.URL)
	defer restoreAPIBase()

	session := connectStripeServer(t)

	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "list_subscriptions",
		Meta: mcp.WithToken("sk_test_whatever"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	var out stripeListSubscriptionsOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Subscriptions) != 1 || out.Subscriptions[0].ID != "sub_1" {
		t.Fatalf("subscriptions = %+v, want one sub_1", out.Subscriptions)
	}
}

// TestStripe_CreateInvoice covers a real bug caught by live manual
// verification 2026-08-28, not present in the original code at all (this
// tool had zero test coverage before): Stripe's POST /v1/invoices errors
// with "invoice_no_customer_line_items" unless the customer already has a
// pending invoice item, so the handler must create one via
// POST /v1/invoiceitems first. This test asserts both calls actually
// happen, in order, with the right fields.
func TestStripe_CreateInvoice(t *testing.T) {
	var gotPaths []string
	var gotItemForm, gotInvoiceForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/invoiceitems":
			gotItemForm = r.Form.Encode()
			_, _ = w.Write([]byte(`{"id":"ii_1"}`))
		case "/invoices":
			gotInvoiceForm = r.Form.Encode()
			_, _ = w.Write([]byte(`{"id":"in_1","status":"draft","hosted_invoice_url":"https://invoice.stripe.com/in_1"}`))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	defer swapStripeAPIBase(srv.URL)()

	session := connectStripeServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "create_invoice",
		Arguments: map[string]any{
			"customer_id":  "cus_1",
			"amount_cents": 500,
			"description":  "Test invoice",
		},
		Meta: mcp.WithToken("sk_test_whatever"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	if len(gotPaths) != 2 || gotPaths[0] != "/invoiceitems" || gotPaths[1] != "/invoices" {
		t.Fatalf("request paths = %v, want [/invoiceitems /invoices] in that order", gotPaths)
	}
	if gotItemForm != "amount=500&currency=usd&customer=cus_1&description=Test+invoice" {
		t.Fatalf("invoice item form = %q", gotItemForm)
	}
	if gotInvoiceForm != "customer=cus_1&description=Test+invoice" {
		t.Fatalf("invoice form = %q", gotInvoiceForm)
	}

	var out stripeCreateInvoiceOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.InvoiceID != "in_1" || out.Status != "draft" {
		t.Fatalf("output = %+v, want invoice_id=in_1 status=draft", out)
	}
}

func TestStripe_CreateInvoice_MissingAmountIsToolError(t *testing.T) {
	session := connectStripeServer(t)

	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "create_invoice",
		Arguments: map[string]any{"customer_id": "cus_1"},
		Meta:      mcp.WithToken("sk_test_whatever"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for a missing required amount_cents")
	}
}

func TestStripe_RefundPayment_MissingIDIsToolError(t *testing.T) {
	session := connectStripeServer(t)

	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "refund_payment",
		Arguments: map[string]any{},
		Meta:      mcp.WithToken("sk_test_whatever"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for a missing required field")
	}
}

func TestStripe_RefundPayment_ForwardsIdempotencyKey(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_1","status":"succeeded"}`))
	}))
	defer srv.Close()
	defer swapStripeAPIBase(srv.URL)()

	session := connectStripeServer(t)
	meta := mcp.WithToken("sk_test_whatever")
	for k, v := range mcp.WithIdempotencyKey("run-abc-2") {
		meta[k] = v
	}

	if _, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "refund_payment",
		Arguments: map[string]any{"payment_intent_id": "pi_1"},
		Meta:      meta,
	}); err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if gotHeader != "run-abc-2" {
		t.Fatalf("Idempotency-Key header = %q, want %q", gotHeader, "run-abc-2")
	}
}

func TestStripe_RefundPayment_NoIdempotencyKeyMeansNoHeader(t *testing.T) {
	var gotHeader string
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader, sawHeader = r.Header.Get("Idempotency-Key"), r.Header.Get("Idempotency-Key") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_1","status":"succeeded"}`))
	}))
	defer srv.Close()
	defer swapStripeAPIBase(srv.URL)()

	session := connectStripeServer(t)
	if _, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "refund_payment",
		Arguments: map[string]any{"payment_intent_id": "pi_1"},
		Meta:      mcp.WithToken("sk_test_whatever"), // no idempotency key set
	}); err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if sawHeader {
		t.Fatalf("Idempotency-Key header = %q, want no header at all when no key was supplied", gotHeader)
	}
}

func TestStripe_RefundPayment_FullVsPartial(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_1","status":"succeeded"}`))
	}))
	defer srv.Close()
	restoreAPIBase := swapStripeAPIBase(srv.URL)
	defer restoreAPIBase()

	session := connectStripeServer(t)
	meta := mcp.WithToken("sk_test_whatever")

	// Full refund: no amount field should be sent.
	if _, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "refund_payment",
		Arguments: map[string]any{"payment_intent_id": "pi_1"},
		Meta:      meta,
	}); err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if gotForm != "payment_intent=pi_1" {
		t.Fatalf("full refund form = %q, want no amount field", gotForm)
	}

	// Partial refund: amount field should be present.
	if _, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "refund_payment",
		Arguments: map[string]any{"payment_intent_id": "pi_1", "amount_cents": 500},
		Meta:      meta,
	}); err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if gotForm != "amount=500&payment_intent=pi_1" {
		t.Fatalf("partial refund form = %q, want amount=500", gotForm)
	}
}

func TestMonthlyNormalizedCents(t *testing.T) {
	tests := []struct {
		name          string
		unitAmount    int64
		quantity      int64
		interval      string
		intervalCount int64
		want          int64
	}{
		{"monthly", 1000, 1, "month", 1, 1000},
		{"yearly", 12000, 1, "year", 1, 1000},
		{"quantity multiplies first", 500, 2, "month", 1, 1000},
		{"quarterly (3-month interval)", 3000, 1, "month", 3, 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := monthlyNormalizedCents(tt.unitAmount, tt.quantity, tt.interval, tt.intervalCount)
			if got != tt.want {
				t.Errorf("monthlyNormalizedCents(%d, %d, %q, %d) = %d, want %d",
					tt.unitAmount, tt.quantity, tt.interval, tt.intervalCount, got, tt.want)
			}
		})
	}
}

// swapStripeAPIBase points stripeAPIBase at a fake server for one test
// and returns a restore func.
func swapStripeAPIBase(url string) func() {
	original := stripeAPIBase
	stripeAPIBase = url
	return func() { stripeAPIBase = original }
}

// unmarshalStructured decodes a CallToolResult's StructuredContent (set
// automatically by AddTool's typed Out value) into out.
func unmarshalStructured(result *gomcp.CallToolResult, out any) error {
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
