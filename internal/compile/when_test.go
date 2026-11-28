package compile

import (
	"strings"
	"testing"
)

func whenData() map[string]any {
	return map[string]any{
		"docker_repo": true,
		"sudo":        false,
		"answer_no":   "no", // text from data that means no
		"env":         "production",
		"uid":         2001,
		"keys":        []any{"a", "b"},
		"empty":       []any{},
		"name":        "",
		"daemon":      map[string]any{"log-driver": "json-file", "live": true},
		"item":        map[string]any{"name": "alice", "admin": true, "in": "x"},
		"facts":       map[string]any{"os": map[string]any{"family": "redhat", "name": "centos"}, "selinux": "enforcing"},
		"host":        map[string]any{"name": "web1", "groups": []any{"web", "docker"}},
	}
}

func TestReadableConditions(t *testing.T) {
	for cond, want := range map[string]bool{
		// One variable, and what counts as true.
		"docker_repo": true, "sudo": false, "keys": true, "empty": false, "name": false, "env": true,
		"item.admin": true, "facts.os.family": true,
		"true": true, "false": false,
		// Text that means no is no, alone and next to and, or, not.
		"answer_no": false, "not answer_no": true, "answer_no or sudo": false, "docker_repo and answer_no": false,
		"answer_no or docker_repo": true, `not answer_no and docker_repo`: true,
		// The right side is not looked at when the left side settles it,
		// so asking first whether something exists works.
		`defined("nothing") and nothing.field == 1`: false, `docker_repo or nothing.field == 1`: true,
		// Comparing.
		`facts.os.family == "redhat"`: true, `facts.os.family == "debian"`: false,
		`facts.os.family != "debian"`: true, `env != "production"`: false,
		`facts.os.family == 'redhat'`: true,
		"uid > 0":                     true, "uid >= 2001": true, "uid < 2001": false, "uid <= 2000": false, "uid == 2001": true,
		"uid > -1": true,
		// and, or, not - and which binds tighter.
		`docker_repo and facts.os.family == "redhat"`:                   true,
		`docker_repo and facts.os.family == "debian"`:                   false,
		`sudo or facts.os.family == "redhat"`:                           true,
		`not sudo`:                                                      true,
		`not docker_repo`:                                               false,
		`not sudo and docker_repo`:                                      true,
		`sudo and docker_repo or true`:                                  true,  // (sudo and docker_repo) or true
		`sudo and (docker_repo or true)`:                                false, // brackets change it
		`not (sudo or env != "production")`:                             true,
		`facts.os.family == "redhat" and facts.selinux != "disabled"`:   true,
		`facts.os.family != "redhat" and facts.os.family != "debian"`:   false,
		`(facts.os.family == "darwin") or (facts.os.name == "centos")`:  true,
		`not not docker_repo`:                                           true,
		`docker_repo and not sudo and uid > 0 and "web" in host.groups`: true,
		`docker_repo   and
		   facts.os.family == "redhat"`: true,
		// in: an item of a list, or a piece of a text.
		`"web" in host.groups`: true, `"db" in host.groups`: false, `"db" not in host.groups`: true,
		`"web" not in host.groups`: false, `"prod" in env`: true,
		`not ("db" in host.groups)`: true,
		`host.groups has "web"`:     true, `host.groups has "db"`: false, `not host.groups has "db"`: true,
		`env has "prod"`: true, `host.groups has "web" and facts.os.family == "redhat"`: true,
		`not host.groups has "db" and docker_repo`: true,
		// Keys that are not plain names, and names that are also words here.
		`daemon["log-driver"] == "json-file"`: true, `daemon["live"]`: true, `keys[0] == "a"`: true,
		`item.in == "x"`: true,
		// Functions, with brackets.
		`defined("item.name")`: true, `defined("item.nothing")`: false, `not defined("nothing")`: true,
		`len(keys) == 2`: true, `hasPrefix("prod", env)`: true, `contains("docker", host.groups)`: true,
		`lower(env) == "production" and len(host.groups) > 1`: true,
	} {
		got, err := evalWhen(cond, whenData())
		if err != nil {
			t.Errorf("%s: %v", cond, err)
		} else if got != want {
			expr, _ := whenExpr(cond)
			t.Errorf("%s: got %v, want %v (read as: %s)", cond, got, want, expr)
		}
	}
}

// Conditions written as template expressions keep working and keep their
// meaning: every form found in the documentation, the examples and the
// modules before the readable form existed.
func TestTemplateConditionsStillWork(t *testing.T) {
	for cond, want := range map[string]bool{
		`eq .facts.os.family "redhat"`:                                      true,
		`ne .facts.os.family "redhat"`:                                      false,
		`contains "web" .host.groups`:                                       true,
		`contains "db" .host.groups`:                                        false,
		`and .docker_repo (eq .facts.os.family "redhat")`:                   true,
		`and (not .sudo) (eq .facts.os.family "redhat")`:                    true,
		`and (eq .facts.os.family "redhat") (ne .facts.selinux "disabled")`: true,
		`or (eq .facts.os.family "darwin") (eq .facts.os.name "centos")`:    true,
		`and (ne .facts.os.family "redhat") (ne .facts.os.family "debian")`: false,
		`or (ne .env "present") (not .sudo)`:                                true,
		`and (eq .env "production") (gt .uid 0)`:                            true,
		`and (eq .env "production") (not (gt .uid 0))`:                      false,
		`and (eq .env "production") .keys`:                                  true,
		`.docker_repo`:                                                      true,
		`.item.admin`:                                                       true,
		`not .docker_repo`:                                                  false,
		`.keys | len | eq 2`:                                                true,
		`defined "item.name"`:                                               true,
		`defined "item.nothing"`:                                            false,
		`eq 1 1`:                                                            true,
		`not true`:                                                          false,
		`{{ if .sudo }}yes{{ end }}`:                                        false,
		`{{ .docker_repo }}`:                                                true,
		`eq (index .daemon "log-driver") "json-file"`:                       true,
		`eq $.env "production"`:                                             true,
	} {
		got, err := evalWhen(cond, whenData())
		if err != nil {
			t.Errorf("%s: %v", cond, err)
		} else if got != want {
			t.Errorf("%s: got %v, want %v", cond, got, want)
		}
		if expr, _ := whenExpr(cond); !strings.Contains(cond, "{{") && cond != "not true" && expr != cond {
			t.Errorf("%s was rewritten to %s", cond, expr)
		}
	}
}

func TestConditionMistakesAreExplained(t *testing.T) {
	for cond, want := range map[string]string{
		`facts.os.family = "redhat"`: "write == to compare",
		`docker_repo && sudo`:        "write and, or, not as words",
		`docker_repo || sudo`:        "write and, or, not as words",
		`!sudo`:                      "write and, or, not as words",
		`docker_repo and`:            "it ends where a value was expected",
		`and docker_repo sudo`:       "", // a template function first: the older form, and its own error
		`docker_repo sudo`:           "did not expect `sudo` here",
		`(docker_repo and sudo`:      "a ( is not closed",
		`facts.os.family == "redhat`: "has no closing quote",
		`facts. == 1`:                "expected a name after the dot",
		`daemon[logdriver] == 1`:     "expected a quoted key or a number",
		`nosuchfunction(env)`:        "there is no function nosuchfunction",
		`uid == "2001"`:              "it compares a number with a text",
		`env == 1`:                   "it compares a text with a number",
		`docker_repoo and sudo`:      `there is no variable or field "docker_repoo"`,
		`facts.os.famly == "redhat"`: `there is no variable or field "famly"`,
		`or sudo`:                    "",
	} {
		_, err := evalWhen(cond, whenData())
		if err == nil {
			t.Errorf("%s: no error", cond)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not say %q", cond, err, want)
		}
	}
}
