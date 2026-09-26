//go:build integration

package documents

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"

	"github.com/founderstack/api/internal/config"
	coredocs "github.com/founderstack/api/internal/core/documents"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// namespacedIndex keeps vectors per Pinecone namespace like the real index;
// leaky ignores namespaces entirely, standing in for a vector-store bug.
type namespacedIndex struct {
	byNamespace map[string][]coredocs.QueryMatch
	ns          string
	leaky       bool
}

func (v namespacedIndex) Namespace(ns string) coredocs.VectorIndex { v.ns = ns; return v }
func (v namespacedIndex) Upsert(ctx context.Context, vectors []*pinecone.Vector) error {
	return nil
}
func (v namespacedIndex) DeleteByID(ctx context.Context, ids []string) error { return nil }
func (v namespacedIndex) Query(ctx context.Context, vector []float32, topK uint32, filter *pinecone.MetadataFilter) ([]coredocs.QueryMatch, error) {
	if v.leaky {
		var all []coredocs.QueryMatch
		for _, m := range v.byNamespace {
			all = append(all, m...)
		}
		return all, nil
	}
	return v.byNamespace[v.ns], nil
}

// twoClientWorkspaces: a practice with two client workspaces, and one
// operator who belongs to all three — the exact shape where a leak between
// workspaces would come from the operator's own session.
func twoClientWorkspaces(t *testing.T, systemPool *pgxpool.Pool) (clerkUserID string, orgA, orgB pgtype.UUID, clerkA, clerkB string) {
	t.Helper()
	ctx := context.Background()
	s := randSuffix(t)
	clerkUserID = "user_documents_operator_" + s
	var practice pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug, organization_type) values ($1, 'Docs Practice', $2, 'practice') returning id",
		"org_documents_practice_"+s, "documents-practice-"+s).Scan(&practice); err != nil {
		t.Fatalf("insert practice: %v", err)
	}
	insertWorkspace := func(label string) (pgtype.UUID, string) {
		var id pgtype.UUID
		clerkID := "org_documents_ws_" + label + "_" + s
		if err := systemPool.QueryRow(ctx,
			`insert into organizations (clerk_org_id, name, slug, organization_type, parent_practice_id)
			 values ($1, $2, $3, 'client_workspace', $4) returning id`,
			clerkID, "Docs Workspace "+label, "documents-ws-"+label+"-"+s, practice).Scan(&id); err != nil {
			t.Fatalf("insert workspace: %v", err)
		}
		return id, clerkID
	}
	orgA, clerkA = insertWorkspace("a")
	orgB, clerkB = insertWorkspace("b")
	for _, org := range []pgtype.UUID{practice, orgA, orgB} {
		if _, err := systemPool.Exec(ctx,
			"insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'operator@example.com', 'admin')",
			org, clerkUserID); err != nil {
			t.Fatalf("insert membership: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = systemPool.Exec(ctx, "delete from organizations where parent_practice_id = $1", practice)
		_, _ = systemPool.Exec(ctx, "delete from organizations where id = $1", practice)
	})
	return clerkUserID, orgA, orgB, clerkA, clerkB
}

func searchAs(t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID, clerkOrgID, query string) (int, []any) {
	t.Helper()
	token, err := devtoken.SignForOrg(cfg.DevTokenSecret.Expose(), clerkUserID, clerkOrgID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/documents/search", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var env apiEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var data struct {
		Results []any `json:"results"`
	}
	_ = json.Unmarshal(env.Data, &data)
	return rec.Code, data.Results
}

func TestDocumentsSearch_ClientWorkspacesNeverLeak(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	operator, orgA, _, clerkA, clerkB := twoClientWorkspaces(t, systemPool)

	docA := insertIndexedDocument(t, appPool, orgA, "acme-financials.pdf", "finance", "all_members")
	vectors := map[string][]coredocs.QueryMatch{
		"org_" + orgA.String(): {fakeMatch(docA, 0, "Acme Q3 revenue was confidential", 0.9)},
	}

	for _, leaky := range []bool{false, true} {
		name := "namespaced vector store"
		if leaky {
			name = "vector store ignoring namespaces"
		}
		t.Run(name, func(t *testing.T) {
			searcher := coredocs.NewSearcher(fakeEmbedder{}, namespacedIndex{byNamespace: vectors, leaky: leaky}, fakeReranker{})
			router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

			code, results := searchAs(t, router, cfg, operator, clerkA, "Acme revenue")
			if code != http.StatusOK || len(results) != 1 {
				t.Fatalf("search in workspace A = (%d, %d results), want the document", code, len(results))
			}
			code, results = searchAs(t, router, cfg, operator, clerkB, "Acme revenue")
			if code != http.StatusOK || len(results) != 0 {
				t.Fatalf("search in workspace B = (%d, %v), want zero results — workspace A's document leaked", code, results)
			}
		})
	}
}
