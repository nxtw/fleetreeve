package pull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A new Vault is initialised with one key and unsealed with it; a wrong
// key leaves it sealed, and that is said.
func TestVaultInitAndUnseal(t *testing.T) {
	initialized, sealed := false, true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/sys/seal-status":
			json.NewEncoder(w).Encode(map[string]any{"initialized": initialized, "sealed": sealed})
		case "/v1/sys/init":
			if initialized || body["secret_shares"] != float64(1) || body["secret_threshold"] != float64(1) {
				http.Error(w, `{"errors":["Vault is already initialized"]}`, 400)
				return
			}
			initialized = true
			json.NewEncoder(w).Encode(map[string]any{"keys_base64": []string{"a2V5"}, "root_token": "root"})
		case "/v1/sys/unseal":
			if body["key"] == "a2V5" {
				sealed = false
			}
			json.NewEncoder(w).Encode(map[string]any{"sealed": sealed})
		case "/v1/sys/health":
			if sealed {
				http.Error(w, `{"sealed":true}`, 503)
			}
		}
	}))
	defer srv.Close()
	v, err := NewVault(srv.URL, "-", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if i, s, err := v.SealStatus(ctx); err != nil || i || !s {
		t.Fatalf("new: initialized %v sealed %v: %v", i, s, err)
	}
	key, root, err := v.Init(ctx)
	if err != nil || key != "a2V5" || root != "root" {
		t.Fatalf("init: %q %q %v", key, root, err)
	}
	if _, _, err := v.Init(ctx); err == nil {
		t.Error("initialised twice")
	}
	if v.Ready(ctx) == nil {
		t.Error("ready while sealed")
	}
	if err := v.Unseal(ctx, "wrong"); err == nil {
		t.Error("a wrong key was not noticed")
	}
	if err := v.Unseal(ctx, key); err != nil {
		t.Fatal(err)
	}
	if i, s, _ := v.SealStatus(ctx); !i || s {
		t.Errorf("after unseal: initialized %v sealed %v", i, s)
	}
	if err := v.Ready(ctx); err != nil {
		t.Error(err)
	}
}
