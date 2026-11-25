package compile

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// funcs returns the template function library. data backs get/defined.
func funcs(data map[string]any) template.FuncMap {
	lookup := func(path string) (any, bool) {
		var cur any = data
		for _, p := range strings.Split(strings.TrimPrefix(path, "."), ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[p]; !ok {
				return nil, false
			}
		}
		return cur, true
	}
	return template.FuncMap{
		// get "a.b" [default]: like .a.b, but tolerates a missing key.
		"get": func(path string, def ...any) (any, error) {
			if v, ok := lookup(path); ok {
				return v, nil
			}
			if len(def) > 0 {
				return def[0], nil
			}
			return nil, fmt.Errorf("get: %q is not defined and no default was given", path)
		},
		"defined": func(path string) bool { _, ok := lookup(path); return ok },
		// list and dict build values in a template, mostly as defaults:
		// `index . "volumes" | default (list "/cache")`.
		"list": func(items ...any) []any { return append([]any{}, items...) },
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict needs pairs of a key and a value")
			}
			m := map[string]any{}
			for i := 0; i < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m, nil
		},
		// required MESSAGE VALUE: the value, or a compile error with the
		// message when it is empty - for settings a template cannot do
		// without, such as a field of a list entry.
		"required": func(msg string, v any) (any, error) {
			if !truthy(v) {
				return nil, fmt.Errorf("%s", msg)
			}
			return v, nil
		},
		"default": func(def, v any) any {
			if truthy(v) {
				return v
			}
			return def
		},
		// Integer arithmetic: {{ div .facts.memory.total_mb 4 }}.
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"mul": func(a, b int) int { return a * b },
		"div": func(a, b int) (int, error) {
			if b == 0 {
				return 0, fmt.Errorf("div: division by zero")
			}
			return a / b, nil
		},
		"mod": func(a, b int) (int, error) {
			if b == 0 {
				return 0, fmt.Errorf("mod: division by zero")
			}
			return a % b, nil
		},
		"lower":     strings.ToLower,
		"upper":     strings.ToUpper,
		"trim":      strings.TrimSpace,
		"replace":   func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
		"hasPrefix": func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
		"hasSuffix": func(suffix, s string) bool { return strings.HasSuffix(s, suffix) },
		"split":     func(sep, s string) []string { return strings.Split(s, sep) },
		"join": func(sep string, list any) (string, error) {
			items, err := toList(list)
			if err != nil {
				return "", fmt.Errorf("join: %v", err)
			}
			parts := make([]string, len(items))
			for i, it := range items {
				parts[i] = fmt.Sprint(it)
			}
			return strings.Join(parts, sep), nil
		},
		// contains ITEM COLLECTION: list membership or substring test.
		"contains": func(item, coll any) bool {
			if s, ok := coll.(string); ok {
				return strings.Contains(s, fmt.Sprint(item))
			}
			items, _ := toList(coll)
			for _, it := range items {
				if fmt.Sprint(it) == fmt.Sprint(item) {
					return true
				}
			}
			return false
		},
		"quote": func(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) },
		"toJson": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"indent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
		},
		"b64enc": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
		"env":    os.Getenv, // environment of the controller
	}
}

// render expands a template string against data. Referencing an undefined
// variable is an error, so typos surface at compile time.
func render(text string, data map[string]any) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	t, err := template.New("").Funcs(funcs(data)).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", cleanErr(err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", cleanErr(err)
	}
	return sb.String(), nil
}

var (
	singleAction = regexp.MustCompile(`^\s*\{\{-?\s*([^{}]+?)\s*-?\}\}\s*$`)
	keyword      = regexp.MustCompile(`^(if|range|with|define|template|block|end|else|break|continue|/\*)\b`)
)

// eval expands a string like render, but when the whole string is a single
// {{ expression }} the result keeps its type (list, map, bool, number)
// instead of being flattened to text.
func eval(text string, data map[string]any) (any, error) {
	m := singleAction.FindStringSubmatch(text)
	if m == nil || keyword.MatchString(m[1]) || strings.Contains(m[1], ":=") {
		return render(text, data)
	}
	v, err := evalExpr(m[1], data)
	if _, isText := v.(string); isText && err == nil {
		// Text is rendered normally, so whitespace around the expression
		// (the newline ending a block scalar) is kept.
		return render(text, data)
	}
	return v, err
}

func evalExpr(expr string, data map[string]any) (any, error) {
	var captured any
	fm := funcs(data)
	fm["__capture"] = func(v any) string { captured = v; return "" }
	t, err := template.New("").Funcs(fm).Option("missingkey=error").Parse("{{ __capture (" + expr + ") }}")
	if err != nil {
		return nil, cleanErr(err)
	}
	if err := t.Execute(&strings.Builder{}, data); err != nil {
		return nil, cleanErr(err)
	}
	return captured, nil
}

// evalWhen evaluates a condition: a bare expression (`eq .facts.os.family
// "debian"`) or a template whose output is tested for truthiness.
func evalWhen(expr string, data map[string]any) (bool, error) {
	var v any
	var err error
	if strings.Contains(expr, "{{") {
		v, err = eval(expr, data)
	} else {
		v, err = evalExpr(expr, data)
	}
	return truthy(v), err
}

// renderAny expands every string in a nested value.
func renderAny(v any, data map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return eval(x, data)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := renderAny(e, data)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", k, err)
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := renderAny(e, data)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

func truthy(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "false", "no", "off", "0":
			return false
		}
		return true
	}
	t, _ := template.IsTrue(v)
	return t
}

// toList converts any slice to []any; a map becomes a list of
// {key, value} pairs sorted by key.
func toList(v any) ([]any, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	case reflect.Map:
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = map[string]any{"key": k.Interface(), "value": rv.MapIndex(k).Interface()}
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a list or map, got %T", v)
}

var errPrefix = regexp.MustCompile(`^template: :\d+(:\d+)?: (executing "" at )?`)

func cleanErr(err error) error {
	return fmt.Errorf("%s", errPrefix.ReplaceAllString(err.Error(), ""))
}

func merge(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		for k, v := range l {
			out[k] = v
		}
	}
	return out
}
