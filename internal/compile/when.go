package compile

import (
	"fmt"
	"strconv"
	"strings"
)

// Conditions (`when:`) can be written the way one reads them:
//
//	when: docker_repo and facts.os.family == "redhat"
//	when: host.groups has "web"
//	when: not (sudo or env != "production")
//
// That is a small language of its own, translated here into the template
// expression the compiler has always evaluated (`and .docker_repo (eq
// .facts.os.family "redhat")`), which stays valid and means what it meant.
//
//	or      a or b
//	and     a and b
//	not     not a
//	compare a == b, a != b, a < b, a <= b, a > b, a >= b,
//	        list has item, item in list, item not in list (also for a piece of a text)
//	value   name, name.field, name["key"], "text", 'text', 12, true, false,
//	        f(a, b), (a)

// whenExpr returns the template expression for a condition. A condition
// already written as a template expression is returned as it is.
func whenExpr(cond string) (string, error) {
	if oldCondition(cond) {
		return cond, nil
	}
	p := &whenParser{src: cond}
	if err := p.lex(); err != nil {
		return "", err
	}
	out, err := p.or()
	if err == nil && p.peek().kind != tEOF {
		err = p.errorf("did not expect %s here", p.peek())
	}
	if err != nil {
		// `eq 1 1`, `defined "item.x"`: a template function with its
		// arguments after it is the older form, without a variable in it.
		if len(p.toks) > 0 && p.toks[0].kind == tIdent && templateFunc(p.toks[0].text) {
			return cond, nil
		}
		return "", err
	}
	return out, nil
}

// oldCondition reports whether a condition is a template expression: it
// names its variables with a leading dot (.facts.os.family), which the
// readable form never does, or uses a pipe or a template variable.
func oldCondition(s string) bool {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '|' && i+1 < len(s) && s[i+1] == '|':
			return false // || is nobody's syntax: let the reader of conditions say so
		case c == '$' || c == '|':
			return true
		case c == '.' && i+1 < len(s) && isIdentStart(s[i+1]):
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '(' {
				return true
			}
		}
	}
	return false
}

// templateFunc reports whether name is a function of the template language.
func templateFunc(name string) bool {
	switch name {
	case "and", "or", "not", "eq", "ne", "lt", "le", "gt", "ge", "len", "index", "print", "printf", "println", "slice", "call", "html", "js", "urlquery":
		return true
	}
	_, ok := funcs(nil)[name]
	return ok && !strings.HasPrefix(name, "__")
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tString
	tNumber
	tOp // == != < <= > >= ( ) [ ] , .
)

type whenTok struct {
	kind tokKind
	text string // for a string: its value, unquoted
	pos  int
}

func (t whenTok) String() string {
	switch t.kind {
	case tEOF:
		return "the end"
	case tString:
		return strconv.Quote(t.text)
	}
	return "`" + t.text + "`"
}

type whenParser struct {
	src  string
	toks []whenTok
	i    int
}

func (p *whenParser) errorf(format string, a ...any) error {
	return fmt.Errorf("cannot read the condition `%s`: %s", strings.TrimSpace(p.src), fmt.Sprintf(format, a...))
}

func (p *whenParser) lex() error {
	s := p.src
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			p.toks = append(p.toks, whenTok{tIdent, s[i:j], i})
			i = j
		case c >= '0' && c <= '9', c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' && j+1 < len(s) && s[j+1] >= '0' && s[j+1] <= '9') {
				j++
			}
			p.toks = append(p.toks, whenTok{tNumber, s[i:j], i})
			i = j
		case c == '"' || c == '\'':
			var val strings.Builder
			j := i + 1
			for ; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					switch s[j] {
					case 'n':
						val.WriteByte('\n')
					case 't':
						val.WriteByte('\t')
					default:
						val.WriteByte(s[j])
					}
					continue
				}
				val.WriteByte(s[j])
			}
			if j >= len(s) {
				return p.errorf("the text starting at %s has no closing quote", s[i:])
			}
			p.toks = append(p.toks, whenTok{tString, val.String(), i})
			i = j + 1
		default:
			op := string(c)
			if i+1 < len(s) && s[i+1] == '=' && (c == '=' || c == '!' || c == '<' || c == '>') {
				op = s[i : i+2]
			}
			switch op {
			case "==", "!=", "<", "<=", ">", ">=", "(", ")", "[", "]", ",", ".":
			case "=":
				return p.errorf("write == to compare (a single = is not a comparison)")
			case "&", "|", "!":
				return p.errorf("write and, or, not as words")
			default:
				return p.errorf("did not expect `%s`", op)
			}
			p.toks = append(p.toks, whenTok{tOp, op, i})
			i += len(op)
		}
	}
	return nil
}

func (p *whenParser) peek() whenTok {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return whenTok{kind: tEOF, pos: len(p.src)}
}

func (p *whenParser) next() whenTok {
	t := p.peek()
	if p.i < len(p.toks) {
		p.i++
	}
	return t
}

// is reports whether the next token is this word or operator.
func (p *whenParser) is(text string) bool {
	t := p.peek()
	return (t.kind == tIdent || t.kind == tOp) && t.text == text
}

func (p *whenParser) accept(text string) bool {
	if p.is(text) {
		p.i++
		return true
	}
	return false
}

func (p *whenParser) or() (string, error) {
	left, err := p.and()
	for err == nil && p.accept("or") {
		var right string
		if right, err = p.and(); err == nil {
			left = "(or " + isTrue(left) + " " + isTrue(right) + ")"
		}
	}
	return left, err
}

func (p *whenParser) and() (string, error) {
	left, err := p.not()
	for err == nil && p.accept("and") {
		var right string
		if right, err = p.not(); err == nil {
			left = "(and " + isTrue(left) + " " + isTrue(right) + ")"
		}
	}
	return left, err
}

func (p *whenParser) not() (string, error) {
	if p.accept("not") {
		inner, err := p.not()
		return "(not " + isTrue(inner) + ")", err
	}
	return p.compare()
}

// isTrue has a value judged the way a condition as a whole is judged, so
// that a variable means the same on its own and next to and, or and not:
// the text "no" is false in both places.
func isTrue(expr string) string { return "(__true " + expr + ")" }

var compareFuncs = map[string]string{"==": "eq", "!=": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge"}

func (p *whenParser) compare() (string, error) {
	left, err := p.value()
	if err != nil {
		return "", err
	}
	t := p.peek()
	switch {
	case t.kind == tOp && compareFuncs[t.text] != "":
		p.next()
		right, err := p.value()
		return "(" + compareFuncs[t.text] + " " + left + " " + right + ")", err
	case p.accept("in"):
		right, err := p.value()
		return "(contains " + left + " " + right + ")", err
	case p.accept("has"): // the same, the other way round: host.groups has "web"
		right, err := p.value()
		return "(contains " + right + " " + left + ")", err
	case p.is("not") && p.i+1 < len(p.toks) && p.toks[p.i+1].kind == tIdent && p.toks[p.i+1].text == "in":
		p.i += 2
		right, err := p.value()
		return "(not (contains " + left + " " + right + "))", err
	}
	return left, nil
}

func (p *whenParser) value() (string, error) {
	t := p.next()
	switch t.kind {
	case tString:
		return strconv.Quote(t.text), nil
	case tNumber:
		return t.text, nil
	case tOp:
		if t.text == "(" {
			inner, err := p.or()
			if err != nil {
				return "", err
			}
			if !p.accept(")") {
				return "", p.errorf("a ( is not closed")
			}
			return inner, nil
		}
	case tIdent:
		switch t.text {
		case "true", "false":
			return t.text, nil
		case "and", "or", "not", "in", "has":
			return "", p.errorf("`%s` needs a value before it", t.text)
		}
		if p.accept("(") {
			return p.call(t.text)
		}
		return p.path(t.text)
	}
	if t.kind == tEOF {
		return "", p.errorf("it ends where a value was expected")
	}
	return "", p.errorf("expected a value, not %s", t)
}

// call reads the arguments of f( ... ).
func (p *whenParser) call(name string) (string, error) {
	if !templateFunc(name) {
		return "", p.errorf("there is no function %s", name)
	}
	out := "(" + name
	for !p.accept(")") {
		if out != "("+name && !p.accept(",") {
			return "", p.errorf("expected , or ) in the arguments of %s, not %s", name, p.peek())
		}
		arg, err := p.or()
		if err != nil {
			return "", err
		}
		out += " " + arg
	}
	return out + ")", nil
}

// path reads a variable: name, name.field.field, name["key"].
func (p *whenParser) path(name string) (string, error) {
	out := "." + name
	for {
		switch {
		case p.accept("."):
			t := p.next()
			if t.kind != tIdent {
				return "", p.errorf("expected a name after the dot, not %s (a key that is not a plain name is written [\"like-this\"])", t)
			}
			out += "." + t.text
		case p.accept("["):
			t := p.next()
			if t.kind != tString && t.kind != tNumber {
				return "", p.errorf("expected a quoted key or a number between [ and ], not %s", t)
			}
			key := t.text
			if t.kind == tString {
				key = strconv.Quote(t.text)
			}
			if !p.accept("]") {
				return "", p.errorf("a [ is not closed")
			}
			out = "(index " + out + " " + key + ")"
		default:
			return out, nil
		}
	}
}
