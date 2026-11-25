package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pull.conf")
	os.WriteFile(p, []byte("# Written by fleetreeve\nFLEETREEVE_SERVER=\"https://fleetreeve.example.com:8140\"\nFLEETREEVE_HOST=\"web \\\"1\\\"\"\nFLEETREEVE_TOKEN_FILE=/etc/fleetreeve/token\nFLEETREEVE_EVERY=30m0s\n\nnot a setting\n"), 0o600)
	got, err := readEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"FLEETREEVE_SERVER": "https://fleetreeve.example.com:8140", "FLEETREEVE_HOST": `web "1"`, "FLEETREEVE_TOKEN_FILE": "/etc/fleetreeve/token", "FLEETREEVE_EVERY": "30m0s"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("extra entries: %v", got)
	}
}
