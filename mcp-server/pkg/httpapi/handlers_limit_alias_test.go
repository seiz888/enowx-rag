package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// The MCP tools call this field `limit`; this endpoint called it `k`. Sending the
// wrong key was silent -- JSON decoding drops unknown fields -- so the caller got
// DefaultK results with no sign that its request had been ignored. Exercised
// through the router so it tests the handler, not a copy of its logic.
func TestSearchAcceptsLimitAsAliasForK(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"limit alone is honoured", `"limit": 2`, 2},
		{"k alone still works", `"k": 3`, 3},
		{"k wins when both are set", `"k": 3, "limit": 2`, 3},
		{"neither falls back to DefaultK", ``, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := make([]rag.Result, 0, 8)
			for i := 0; i < 8; i++ {
				res = append(res, rag.Result{
					ID:      fmt.Sprintf("r%d", i),
					Content: fmt.Sprintf("result %d", i),
					Score:   1 - float64(i)/10,
				})
			}
			p := &mockProvider{projects: []string{"proj1"}, searchResults: res}
			_, router := newTestServer(t, p, nil)

			body := `{"project_id": "proj1", "query": "test"`
			if tc.body != "" {
				body += ", " + tc.body
			}
			body += "}"
			req := httptest.NewRequest(http.MethodPost, "/api/search",
				strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Results []rag.Result `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(resp.Results) != tc.want {
				t.Errorf("got %d results, want %d", len(resp.Results), tc.want)
			}
		})
	}
}
