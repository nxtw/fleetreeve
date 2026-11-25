package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// Logging. Every command accepts -log-format text|json (or
// $FLEETREEVE_LOG_FORMAT). `fleetreeve serve` logs JSON by default, as do the pull
// services that enroll/install and the RPM set up; other commands default
// to output meant for people.
//
// In JSON mode each line is one object with time, level and msg plus
// fields: run events (one per resource, one per host) go to stdout,
// operational messages and errors to stderr.
var (
	jsonLogs bool
	runLog   = slog.New(slog.NewJSONHandler(os.Stdout, nil)) // run events
	opLog    = slog.New(slog.NewTextHandler(os.Stderr, nil)) // operational messages
)

func setLogFormat(format string) error {
	switch format {
	case "json":
		jsonLogs = true
		opLog = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	case "text":
		jsonLogs = false
		opLog = slog.New(slog.NewTextHandler(os.Stderr, nil))
	default:
		return fmt.Errorf("-log-format must be text or json, not %q", format)
	}
	return nil
}

// notef reports something worth knowing outside the run itself: a
// warning, a fallback, a fatal error. People get "fleetreeve: message"; JSON
// mode gets a log record.
func notef(level slog.Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if jsonLogs {
		opLog.Log(context.Background(), level, msg)
		return
	}
	fmt.Fprintf(os.Stderr, "fleetreeve: %s\n", msg)
}
