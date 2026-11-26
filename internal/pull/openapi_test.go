package pull

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The API reference must describe every endpoint the server serves.
func TestOpenAPICoversRoutes(t *testing.T) {
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("api/openapi.yaml does not parse: %v", err)
	}
	routes := map[string][]string{
		"/v1/health":                          {"get"},
		"/v1/status":                          {"get"},
		"/v1/hosts":                           {"get"},
		"/v1/hosts/{name}":                    {"get", "put", "delete"},
		"/v1/hosts/{name}/reports/{manifest}": {"get"},
		"/v1/hosts/{name}/actions":            {"post"},
		"/v1/hosts/{name}/actions/{id}":       {"delete"},
		"/v1/actions/claim":                   {"post"},
		"/v1/actions/results":                 {"post"},
		"/v1/actions":                         {"get"},
		"/v1/history":                         {"get"},
		"/v1/tokens":                          {"post"},
		"/v1/join-tokens":                     {"post"},
		"/v1/catalog":                         {"post"},
		"/v1/report":                          {"post"},
		"/v1/join":                            {"post"},
		"/v1/cert":                            {"post"},
		"/v1/certs":                           {"get"},
		"/v1/certs/{name}":                    {"delete"},
		"/v1/agent/{platform}":                {"get"},
		"/v1/package/{package}":               {"get"},
		"/install.sh":                         {"get"},
	}
	for path, methods := range routes {
		for _, m := range methods {
			if _, ok := doc.Paths[path][m]; !ok {
				t.Errorf("api/openapi.yaml lacks %s %s", m, path)
			}
		}
	}
	if len(doc.Paths) != len(routes) {
		t.Errorf("api/openapi.yaml has %d paths, the server %d: one of them is out of date", len(doc.Paths), len(routes))
	}
}
