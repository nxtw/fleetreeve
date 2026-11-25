package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/nxtw/fleetreeve/internal/dashboard"
	"github.com/nxtw/fleetreeve/internal/pull"
)

// dashboardCmd serves the web console: a read-only view of a fleetreeve server,
// run as its own process or container.
func dashboardCmd(ctx context.Context, o *options) error {
	// A container is configured through its environment.
	if o.server == "" {
		o.server = os.Getenv("FLEETREEVE_SERVER")
	}
	if o.ca == "" {
		o.ca = os.Getenv("FLEETREEVE_CA")
	}
	if o.users == "" {
		o.users = os.Getenv("FLEETREEVE_DASHBOARD_USERS")
	}
	accounts, err := dashboard.NewAccounts(o.users, os.Getenv("FLEETREEVE_DASHBOARD_AUTH"))
	if err != nil {
		return err
	}
	d := &dashboard.Dashboard{Server: o.server, Every: o.every, Users: accounts, Log: opLog}
	switch {
	case o.demo:
		d.Source, d.Demo = dashboard.NewDemoSource(), true
		if d.Every == 0 {
			d.Every = 5 * time.Second
		}
	case o.server == "":
		return fmt.Errorf("usage: fleetreeve dashboard -server <url> (-secret-file <file> | $FLEETREEVE_API_TOKEN) [-ca file] [-listen :8141] [-tls-cert file -tls-key file]\n" +
			"       fleetreeve dashboard -demo        (generated data, to look around)")
	default:
		token := os.Getenv("FLEETREEVE_API_TOKEN")
		if token == "" && o.secretFile != "" {
			secret, _, err := pull.LoadSecret(o.secretFile, false)
			if err != nil {
				return err
			}
			token = pull.APIToken(secret, "read")
		}
		if token == "" {
			return fmt.Errorf("dashboard needs the server's read token: $FLEETREEVE_API_TOKEN (fleetreeve token -api read), or -secret-file to derive it")
		}
		var client *pull.Client
		client, err = pull.NewClient(o.server, "", "", o.ca, o.insecure)
		if err != nil {
			return err
		}
		// Optional: lets the console queue rollbacks. Without it, read-only.
		// FLEETREEVE_EXEC_TOKEN, also optional, lets admins run shell commands.
		d.Source = dashboard.NewAPISource(client, token, os.Getenv("FLEETREEVE_OPERATOR_TOKEN"), os.Getenv("FLEETREEVE_EXEC_TOKEN"))
	}
	if (o.tlsCert == "") != (o.tlsKey == "") {
		return fmt.Errorf("-tls-cert and -tls-key must be given together")
	}
	listen := o.listen
	if listen == ":8140" { // the server's default; the dashboard has its own
		listen = ":8141"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go d.Run(ctx)
	srv := &http.Server{ErrorLog: slog.NewLogLogger(opLog.Handler(), slog.LevelWarn), Handler: d.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: time.Minute}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	scheme := "http"
	if o.tlsCert != "" {
		scheme = "https"
	}
	canAct := d.Source.CanAct() && (d.Users != nil || d.Demo)
	opLog.Info("dashboard listening", "url", scheme+"://"+ln.Addr().String(), "server", o.server, "demo", o.demo, "accounts", d.Users != nil, "runs_and_rollback", canAct,
		"commands", d.Source.CanExec() && (d.Users != nil || d.Demo))
	if d.Source.CanAct() && !canAct {
		opLog.Warn("FLEETREEVE_OPERATOR_TOKEN is set but the console has no accounts: rollback stays off until it has (fleetreeve user add, with -users or FLEETREEVE_DASHBOARD_USERS)")
	}
	if d.Users == nil {
		opLog.Warn("the console has no accounts: anyone who can reach this port can read the fleet's status (create accounts with `fleetreeve user add` and pass the file with -users, or put it behind your own sign-on)")
	}
	if o.tlsCert != "" {
		err = srv.ServeTLS(ln, o.tlsCert, o.tlsKey)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
