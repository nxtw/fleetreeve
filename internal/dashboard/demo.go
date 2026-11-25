package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
	"github.com/nxtw/fleetreeve/internal/engine"
	"github.com/nxtw/fleetreeve/internal/pull"
	"net/url"
	"strconv"
)

// demoSource generates a plausible fleet, so the dashboard can be looked at
// without a server. Hosts keep their character between polls; a few change
// state over time so the page visibly lives.
type demoSource struct {
	start time.Time
	mu    *sync.Mutex
	queue map[string][]pull.Action // per host
	done  map[string][]pull.ActionResult
}

// NewDemoSource returns generated data. The UI marks it clearly as such.
func NewDemoSource() Source {
	return demoSource{start: time.Now(), mu: &sync.Mutex{}, queue: map[string][]pull.Action{}, done: map[string][]pull.ActionResult{}}
}

func (d demoSource) CanAct() bool  { return true }
func (d demoSource) CanExec() bool { return true }

func (d demoSource) SendExec(ctx context.Context, path string, body any) ([]byte, error) {
	return d.Send(ctx, "POST", path, body)
}

// actions pretends hosts pick requests up after a few seconds.
func (d demoSource) actions() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pending, results := []pull.Action{}, []pull.ActionResult{}
	for host, list := range d.queue {
		var keep []pull.Action
		for _, a := range list {
			queued, _ := time.Parse(time.RFC3339, a.Queued)
			if a.Kind == "rollback" || a.Kind == "release" || time.Since(queued) < 4*time.Second {
				keep = append(keep, a)
				pending = append(pending, a)
				continue
			}
			res := pull.ActionResult{Action: a, Status: "done", Done: time.Now().UTC().Format(time.RFC3339)}
			switch a.Kind {
			case "apply":
				res.Detail = "48 resources · in sync  (1.2s)"
			case "plan":
				res.Detail = "48 resources · 1 to change  (0.9s)"
			case "command":
				res.Output = "(demo) nothing was run: " + a.Command + "\n" + host + " would answer here\n"
			}
			d.done[host] = append([]pull.ActionResult{res}, d.done[host]...)
		}
		d.queue[host] = keep
	}
	for _, list := range d.done {
		results = append(results, list...)
	}
	return json.Marshal(map[string]any{"pending": pending, "results": results})
}

// Send pretends to queue: the request shows up as waiting, and nothing
// ever collects it.
func (d demoSource) Send(_ context.Context, method, path string, body any) ([]byte, error) {
	if method == "GET" && path == "/v1/actions" {
		return d.actions()
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/hosts/"), "/")
	d.mu.Lock()
	defer d.mu.Unlock()
	if method == "DELETE" && len(parts) == 3 {
		var keep []pull.Action
		for _, a := range d.queue[parts[0]] {
			if a.ID != parts[2] {
				keep = append(keep, a)
			}
		}
		d.queue[parts[0]] = keep
		return nil, nil
	}
	a, _ := body.(pull.Action)
	a.ID, a.Queued = fmt.Sprintf("demo%d", time.Now().UnixNano()), time.Now().UTC().Format(time.RFC3339)
	d.queue[a.Host] = append(d.queue[a.Host], a)
	return json.Marshal(a)
}

type demoHost struct {
	name, manifest string
	groups         []string
	resources      int
}

func demoFleet() []demoHost {
	var out []demoHost
	add := func(prefix, group, site string, n, resources int) {
		for i := 1; i <= n; i++ {
			out = append(out, demoHost{fmt.Sprintf("%s-%s-%02d", prefix, site, i), "site", []string{group, site}, resources})
		}
	}
	for _, site := range []string{"ams", "fra", "nyc"} {
		add("web", "web", site, 8, 35)
		add("app", "app", site, 6, 48)
		add("db", "db", site, 3, 36)
		add("lb", "lb", site, 2, 28)
	}
	add("build", "ci", "ams", 4, 22)
	add("mon", "monitoring", "fra", 2, 31)
	return out
}

func hashOf(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func (d demoSource) report(h demoHost, now time.Time) (*pull.Report, bool) {
	seed := hashOf(h.name)
	tick := uint32(now.Sub(d.start) / (45 * time.Second))
	roll := (seed + tick*uint32(7+seed%5)) % 100
	if seed%41 == 0 {
		return nil, false // never reported
	}
	interval := 1800
	age := time.Duration(seed%1700) * time.Second
	rep := &pull.Report{Host: h.name, Manifest: h.manifest, Source: "pull", Mode: "apply", Version: "0.1.0", Interval: interval}
	rep.Summary = engine.Summary{Total: h.resources, OK: h.resources, Millis: int64(180 + seed%900)}
	switch {
	case seed%29 == 0: // gone quiet
		age = time.Duration(2*interval+900+int(seed%4000)) * time.Second
	case roll < 4: // a failing resource
		rep.Summary.OK, rep.Summary.Failed, rep.Summary.Skipped = h.resources-3, 1, 2
		rep.Results = []engine.Result{
			{ID: "package[nginx]", Status: engine.Failed, Error: "dnf install: exit status 1: Error: Failed to download metadata for repo 'appstream'", Origin: "roles/web.yaml:9"},
			{ID: "file[/etc/nginx/nginx.conf]", Status: engine.Skipped, Error: "dependency package[nginx] failed", Origin: "roles/web.yaml:12"},
			{ID: "service[nginx]", Status: engine.Skipped, Error: "dependency file[/etc/nginx/nginx.conf] was skipped", Origin: "roles/web.yaml:49"},
		}
	case roll < 16: // converged something on its last run
		rep.Summary.OK, rep.Summary.Changed, rep.Summary.Refreshed = h.resources-2, 2, 1
		rep.Results = []engine.Result{
			{ID: "file[/etc/motd]", Status: engine.Changed, Drifted: true, Origin: "roles/base.yaml:24",
				Changes: []core.Change{{Prop: "content", From: "sha256:92e78d0b0329", To: "sha256:f7877a4c8500"}}},
			{ID: "file[/etc/ssh/sshd_config.d/00-fleetreeve.conf]", Status: engine.Changed, Origin: "roles/ssh.yaml:25",
				Changes: []core.Change{{Prop: "mode", From: "0644", To: "0600"}}},
			{ID: "service[sshd]", Status: engine.OK, Refresh: true, Origin: "roles/ssh.yaml:37"},
		}
		age = time.Duration(seed%240) * time.Second
	}
	if strings.HasPrefix(h.name, "build-") { // CI hosts are only audited
		rep.Mode, rep.Source, rep.Interval = "plan", "push", 0
		if seed%2 == 0 {
			rep.Summary.OK, rep.Summary.Pending = h.resources-1, 1
			rep.Results = []engine.Result{{ID: "file[/etc/sysctl.d/60-vm.swappiness.conf]", Status: engine.Pending, Origin: "roles/base.yaml:31",
				Changes: []core.Change{{Prop: "content", From: "sha256:1c69921c5f29", To: "sha256:b6a366126213"}}}}
		}
	}
	rep.Time = now.Add(-age).UTC().Format(time.RFC3339)
	rep.Results = demoApplied(h, rep.Results, now.Add(-age))
	// What the node says it could roll back.
	changed := now.Add(-age - time.Duration(seed%96)*time.Hour).UTC().Format(time.RFC3339)
	rep.Restorable = []engine.RollbackResult{
		{ID: "file[/etc/nginx/conf.d/www.conf]", Status: "kept", Time: changed, Remaining: 2,
			Changes: []core.Change{{Prop: "content", From: "sha256:529c7e1deb05", To: "sha256:dac1e962bcb1"}}},
		{ID: "file[/etc/motd]", Status: "kept", Time: changed, Remaining: 1,
			Changes: []core.Change{{Prop: "content", From: "sha256:1f29b2bea1b2", To: "sha256:c351fe14dd3f"}}},
	}
	if seed%7 == 0 {
		rep.Restorable = append(rep.Restorable, engine.RollbackResult{ID: "file[/etc/haproxy/haproxy.cfg]", Status: "kept", Held: changed})
		rep.Results = demoApplied(h, []engine.Result{{ID: "file[/etc/haproxy/haproxy.cfg]", Status: engine.OK, Held: true, Origin: "roles/loadbalancer.yaml:9"}}, now.Add(-age))
		rep.Summary.Held = 1
	}
	return rep, true
}

// demoPool is what the demo's roles and modules manage, per group.
var demoPool = map[string][]string{
	"": {
		"package[chrony]|roles/base.yaml:8", "service[chronyd]|roles/base.yaml:11", "file[/etc/localtime]|roles/base.yaml:16", "file[/etc/motd]|roles/base.yaml:24",
		"file[/etc/sysctl.d/60-vm.swappiness.conf]|roles/base.yaml:31", "exec[reload sysctl]|roles/base.yaml:38", "package[rsync]|roles/base.yaml:44", "package[tar]|roles/base.yaml:44",
		"group[admins]|roles/admins.yaml:5", "user[alice]|roles/admins.yaml:9", "user[bob]|roles/admins.yaml:9", "file[/home/alice/.ssh/authorized_keys]|roles/admins.yaml:21",
		"file[/home/bob/.ssh/authorized_keys]|roles/admins.yaml:21", "file[/etc/sudoers.d/admins]|roles/admins.yaml:30",
		"file[/etc/ssh/sshd_config.d/00-fleetreeve.conf]|roles/ssh.yaml:25", "service[sshd]|roles/ssh.yaml:37",
		"package[firewalld]|modules/fleetreeve.firewalld/roles/main.yaml:3", "service[firewalld]|modules/fleetreeve.firewalld/roles/main.yaml:6", "fleetreeve.firewalld.port[22/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11",
		"package[policycoreutils-python-utils]|roles/selinux.yaml:4", "exec[selinux boolean httpd_can_network_connect]|roles/selinux.yaml:9",
	},
	"web": {
		"package[nginx]|modules/fleetreeve.nginx/roles/main.yaml:3", "file[/etc/nginx/nginx.conf]|modules/fleetreeve.nginx/roles/main.yaml:6", "exec[check nginx configuration]|modules/fleetreeve.nginx/roles/main.yaml:17",
		"service[nginx]|modules/fleetreeve.nginx/roles/main.yaml:23", "file[/usr/share/nginx/html/www]|modules/fleetreeve.nginx/types/vhost.yaml:9", "file[/usr/share/nginx/html/www/index.html]|modules/fleetreeve.nginx/types/vhost.yaml:12",
		"file[/etc/nginx/conf.d/www.conf]|modules/fleetreeve.nginx/types/vhost.yaml:16", "file[/usr/share/nginx/html/shop]|modules/fleetreeve.nginx/types/vhost.yaml:9", "file[/usr/share/nginx/html/shop/index.html]|modules/fleetreeve.nginx/types/vhost.yaml:12",
		"file[/etc/nginx/conf.d/shop.conf]|modules/fleetreeve.nginx/types/vhost.yaml:16", "fleetreeve.firewalld.port[80/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11", "fleetreeve.firewalld.port[443/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11",
		"file[/etc/logrotate.d/nginx]|roles/web.yaml:18", "file[/etc/nginx/conf.d/status.conf]|roles/web.yaml:24",
	},
	"lb": {
		"package[haproxy]|roles/loadbalancer.yaml:4", "file[/etc/haproxy/haproxy.cfg]|roles/loadbalancer.yaml:9", "service[haproxy]|roles/loadbalancer.yaml:20",
		"fleetreeve.firewalld.port[80/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11", "fleetreeve.firewalld.port[443/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11",
		"exec[selinux boolean haproxy_connect_any]|roles/selinux.yaml:9", "file[/etc/rsyslog.d/49-haproxy.conf]|roles/loadbalancer.yaml:27",
	},
	"db": {
		"package[postgresql-server]|roles/postgres.yaml:5", "exec[initialise postgres]|roles/postgres.yaml:9", "file[/var/lib/pgsql/data/postgresql.conf]|roles/postgres.yaml:15",
		"file[/var/lib/pgsql/data/pg_hba.conf]|roles/postgres.yaml:24", "service[postgresql]|roles/postgres.yaml:33", "file[/usr/local/bin/pg-backup.sh]|roles/postgres.yaml:40",
		"file[/etc/cron.d/pg-backup]|roles/postgres.yaml:47", "file[/var/backups/postgres]|roles/postgres.yaml:53", "fleetreeve.firewalld.port[5432/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11",
		"user[backup]|roles/postgres.yaml:58", "line[vm.overcommit_memory]|roles/postgres.yaml:63", "file[/etc/systemd/system/postgresql.service.d/limits.conf]|roles/postgres.yaml:69",
		"package[pg_stat_monitor]|roles/postgres.yaml:76", "file[/etc/logrotate.d/postgresql]|roles/postgres.yaml:80", "exec[reload postgres]|roles/postgres.yaml:86",
	},
	"app": {
		"package[java-21-openjdk-headless]|roles/app.yaml:4", "user[app]|roles/app.yaml:8", "file[/opt/app]|roles/app.yaml:13", "file[/opt/app/config]|roles/app.yaml:13",
		"file[/opt/app/config/application.yaml]|roles/app.yaml:19", "file[/etc/systemd/system/app.service]|roles/app.yaml:28", "exec[reload systemd]|roles/app.yaml:35", "service[app]|roles/app.yaml:40",
		"file[/etc/logrotate.d/app]|roles/app.yaml:46", "fleetreeve.firewalld.port[8080/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11", "file[/var/log/app]|roles/app.yaml:52",
		"line[app ulimit]|roles/app.yaml:57", "file[/opt/app/bin/healthcheck.sh]|roles/app.yaml:63", "file[/etc/cron.d/app-healthcheck]|roles/app.yaml:69",
		"package[jq]|roles/app.yaml:74", "file[/opt/app/config/logback.xml]|roles/app.yaml:78", "file[/etc/sysconfig/app]|roles/app.yaml:85",
		"file[/opt/app/config/secrets.env]|roles/app.yaml:91", "group[app]|roles/app.yaml:7", "file[/opt/app/releases]|roles/app.yaml:13",
		"file[/etc/tmpfiles.d/app.conf]|roles/app.yaml:97", "exec[create tmpfiles]|roles/app.yaml:102", "package[unzip]|roles/app.yaml:74",
		"file[/etc/sudoers.d/app-deploy]|roles/app.yaml:107", "user[deploy]|roles/app.yaml:112", "file[/home/deploy/.ssh/authorized_keys]|roles/app.yaml:117", "file[/etc/profile.d/app.sh]|roles/app.yaml:124",
	},
	"ci": {
		"file[/etc/yum.repos.d/runner_gitlab-runner.repo]|modules/fleetreeve.gitlab_runner/roles/main.yaml:12", "package[gitlab-runner]|modules/fleetreeve.gitlab_runner/roles/main.yaml:17",
		"line[gitlab-runner concurrent]|modules/fleetreeve.gitlab_runner/roles/main.yaml:38", "fleetreeve.gitlab_runner.runner[build]|modules/fleetreeve.gitlab_runner/roles/main.yaml:52", "service[gitlab-runner]|modules/fleetreeve.gitlab_runner/roles/main.yaml:85",
		"package[podman]|modules/fleetreeve.gitlab_runner/roles/main.yaml:92", "service[podman.socket]|modules/fleetreeve.gitlab_runner/roles/main.yaml:95",
	},
	"monitoring": {
		"package[prometheus]|roles/monitoring.yaml:4", "file[/etc/prometheus/prometheus.yml]|roles/monitoring.yaml:8", "service[prometheus]|roles/monitoring.yaml:17", "package[grafana]|roles/monitoring.yaml:22",
		"file[/etc/grafana/grafana.ini]|roles/monitoring.yaml:26", "service[grafana-server]|roles/monitoring.yaml:34", "fleetreeve.firewalld.port[3000/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11",
		"fleetreeve.firewalld.port[9090/tcp]|modules/fleetreeve.firewalld/roles/main.yaml:11", "file[/etc/prometheus/rules/fleet.yml]|roles/monitoring.yaml:40", "exec[reload prometheus]|roles/monitoring.yaml:47",
	},
}

// demoApplied is the full list of what a demo host is kept to: its
// groups' resources, with the results of the last run laid over them.
func demoApplied(h demoHost, notable []engine.Result, ran time.Time) []engine.Result {
	seed := hashOf(h.name)
	var out []engine.Result
	at := map[string]int{}
	for _, g := range append([]string{""}, h.groups...) {
		for _, item := range demoPool[g] {
			id, origin, _ := strings.Cut(item, "|")
			if _, dup := at[id]; dup || len(out) >= h.resources {
				continue
			}
			res := engine.Result{ID: id, Status: engine.OK, Origin: origin}
			// Most things were set up when the host was built; some were
			// changed since; packages and the like were already there.
			switch k := hashOf(id+h.name) % 10; {
			case k < 5:
				res.Since, res.Created = ran.Add(-time.Duration(200+seed%90)*24*time.Hour).UTC().Format(time.RFC3339), !strings.HasPrefix(id, "service[")
			case k < 7:
				res.Since = ran.Add(-time.Duration(1+hashOf(id)%60) * 24 * time.Hour).UTC().Format(time.RFC3339)
			}
			at[id] = len(out)
			out = append(out, res)
		}
	}
	for _, n := range notable {
		if n.Status == engine.Changed {
			n.Since = ran.UTC().Format(time.RFC3339)
		}
		if i, ok := at[n.ID]; ok {
			if n.Since == "" {
				n.Since, n.Created = out[i].Since, out[i].Created
			}
			out[i] = n
		} else {
			out = append(out, n)
		}
	}
	return out
}

func (d demoSource) Get(_ context.Context, path string) ([]byte, error) {
	now := time.Now()
	fleet := demoFleet()
	var reports []pull.Report
	hosts := make([]pull.HostInfo, 0, len(fleet))
	var expected []string
	byHost := map[string]*pull.Report{}
	for _, h := range fleet {
		expected = append(expected, h.name)
		info := pull.HostInfo{Name: h.name, Groups: h.groups, Source: "inventory", Reports: []pull.StatusEntry{}}
		if hashOf(h.name)%5 == 0 {
			info.Source, info.Registered = "registered", now.Add(-time.Duration(hashOf(h.name)%900)*time.Hour).UTC().Format(time.RFC3339)
		}
		if rep, ok := d.report(h, now); ok {
			reports = append(reports, *rep)
			byHost[h.name] = rep
			info.Reports = append(info.Reports, rep.Entry(now))
		}
		hosts = append(hosts, info)
	}
	switch {
	case strings.HasPrefix(path, "/v1/history"):
		return demoHistory(path, fleet, now)
	case path == "/v1/health":
		return json.Marshal(map[string]string{"status": "ok", "version": "0.1.0"})
	case path == "/v1/status":
		return json.Marshal(pull.Status(reports, expected, now))
	case path == "/v1/hosts":
		return json.Marshal(map[string]any{"hosts": hosts})
	case strings.HasPrefix(path, "/v1/hosts/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/hosts/"), "/")
		for _, h := range hosts {
			if h.Name != parts[0] {
				continue
			}
			if len(parts) == 1 {
				d.mu.Lock()
				h.Pending = d.queue[h.Name]
				d.mu.Unlock()
				return json.Marshal(h)
			}
			if rep := byHost[h.Name]; rep != nil {
				return json.Marshal(rep)
			}
		}
	}
	return nil, fmt.Errorf("no such host")
}

// demoHistory makes up a log: one event per 40 seconds, going back two
// weeks, the same whenever it is asked for.
func demoHistory(path string, fleet []demoHost, now time.Time) ([]byte, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	const step = 40
	slot := now.Unix() / step
	if b := q.Get("before"); b != "" {
		if slot, err = strconv.ParseInt(b, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid cursor %q", b)
		}
		slot--
	}
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 1000)
	}
	oldest := now.Unix()/step - 14*24*3600/step
	page := pull.HistoryPage{Events: []pull.Event{}}
	for ; slot > oldest; slot-- {
		seed := hashOf(fmt.Sprint("slot", slot))
		h := fleet[seed%uint32(len(fleet))]
		ev := pull.Event{Time: time.Unix(slot*step+int64(seed%step), 0).UTC().Format(time.RFC3339), Host: h.name, Kind: "run",
			Manifest: h.manifest, Source: "pull", Mode: "apply", State: "ok"}
		sum := engine.Summary{Total: h.resources, OK: h.resources, Millis: int64(180 + seed%900)}
		switch roll := seed / 7 % 100; {
		case roll < 3:
			ev.Kind, ev.Request, ev.RequestedBy, ev.Status = "request", []string{"apply", "plan", "rollback", "release", "command"}[seed/3%5], "ann (console)", "done"
			if ev.Request == "rollback" || ev.Request == "release" {
				ev.Resource = "file[/etc/motd]"
			}
			if ev.Request == "command" {
				exit := int(seed / 11 % 2)
				ev.Exit = &exit
			}
		case roll < 5:
			ev.State = "failed"
			sum.OK, sum.Failed, sum.Skipped = h.resources-3, 1, 2
			ev.Changes = []pull.EventChange{
				{ID: "package[nginx]", Status: "failed", Error: "dnf install: exit status 1: Error: Failed to download metadata for repo 'appstream'"},
				{ID: "file[/etc/nginx/nginx.conf]", Status: "skipped", Error: "dependency package[nginx] failed"},
				{ID: "service[nginx]", Status: "skipped", Error: "dependency file[/etc/nginx/nginx.conf] was skipped"}}
		case roll < 16:
			ev.State = "changed"
			sum.OK, sum.Changed, sum.Refreshed = h.resources-2, 2, 1
			ev.Changes = []pull.EventChange{
				{ID: "file[/etc/motd]", Status: "changed", Drifted: true, Props: []string{"content: sha256:92e78d0b0329 -> sha256:f7877a4c8500"}},
				{ID: "file[/etc/ssh/sshd_config.d/00-fleetreeve.conf]", Status: "changed", Props: []string{"mode: 0644 -> 0600"}},
				{ID: "service[sshd]", Status: "ok", Refresh: true}}
		case roll < 18:
			ev.Mode, ev.Source, ev.State = "plan", "push", "drift"
			sum.OK, sum.Pending = h.resources-1, 1
			ev.Changes = []pull.EventChange{{ID: "file[/etc/sysctl.d/60-vm.swappiness.conf]", Status: "pending", Props: []string{"content: sha256:1c69921c5f29 -> sha256:b6a366126213"}}}
		}
		if ev.Kind == "run" {
			ev.Summary = &sum
		}
		if (q.Get("host") != "" && ev.Host != q.Get("host")) || (q.Get("only") == "changes" && !ev.Eventful()) {
			continue
		}
		page.Events = append(page.Events, ev)
		if len(page.Events) == limit {
			page.Next = strconv.FormatInt(slot, 10)
			break
		}
	}
	return json.Marshal(page)
}
