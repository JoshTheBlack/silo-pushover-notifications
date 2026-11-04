package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The OpenAPI document is served, parses, and lists only routes the plugin
// answers.
func TestOpenAPIDocumentMatchesRoutes(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)

	status, doc := h.request(t, http.MethodGet, "/api/openapi.json", asKid, nil)
	if status != http.StatusOK || doc["openapi"] != "3.1.0" {
		t.Fatalf("GET /api/openapi.json = %d %v", status, doc["openapi"])
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Paths) < 10 {
		t.Fatalf("only %d paths", len(spec.Paths))
	}
	fill := strings.NewReplacer("{profile_id}", "p1", "{session_id}", "s1")
	for p, ops := range spec.Paths {
		for method := range ops {
			if method == "parameters" {
				continue
			}
			m := strings.ToUpper(method)
			var body any
			if m == http.MethodPost || m == http.MethodPut {
				body = map[string]any{}
			}
			if m == http.MethodDelete {
				continue // removing settings would change later calls
			}
			status, out := h.request(t, m, fill.Replace(p), asPrimary, body)
			if status == http.StatusMethodNotAllowed || (status == http.StatusNotFound && out["code"] == "not_found") {
				t.Errorf("%s %s is in the OpenAPI document but answers %d %v", m, p, status, out)
			}
		}
	}
}
