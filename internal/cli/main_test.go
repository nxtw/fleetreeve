package cli

import (
	"os"
	"testing"
)

// Tests must not pick up a settings file from the project or the home
// directory of whoever runs them.
func TestMain(m *testing.M) {
	os.Setenv("FLEETREEVE_CONFIG", "none")
	os.Setenv("FLEETREEVE_NODE_CONF", "none") // tests must not report to a server this machine happens to be enrolled with
	os.Exit(m.Run())
}
