package i18n

import (
	"fmt"
	"regexp"
	"strings"
)

// Key names a message, such as NOT_FOUND. Every key is defined in
// catalogs/en.json, and translated in the other catalogs.
type Key string

var keyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Message is a message to show a person: its key and the values of its
// parameters. Handlers send messages, never texts, so that every text is
// in the catalogs and each language orders the parameters as it needs.
type Message struct {
	Key  Key
	Args []Arg
}

// Arg is the value of a named parameter of a message.
type Arg struct {
	Name string
	// Value is a Key or a Message, which is rendered in the same locale,
	// such as OPERATION_BUY for "Buying" or «خرید», or a literal, such as
	// a currency symbol or a number, which is inserted as it is.
	Value any
}

// M returns the message of key with args.
func M(key Key, args ...Arg) Message {
	return Message{Key: key, Args: args}
}

// A returns the value of the parameter name.
func A(name string, value any) Arg {
	return Arg{Name: name, Value: value}
}

// IsZero reports a message with no key.
func (m Message) IsZero() bool {
	return m.Key == ""
}

// String renders the message in English, for logs.
func (m Message) String() string {
	text, _ := render(English, catalogs[English], m, false)
	return text
}

// The Unicode isolates that keep a literal in its own direction inside a
// text of the other direction: an address, a symbol such as BTC/USDT, or a
// number inside Persian text.
const (
	firstStrongIsolate = "\u2068"
	popDirectionalIsol = "\u2069"
)

// Render returns m in locale l, from the catalog of l, or else from the
// English one, and the language that the text is in. A key that no catalog
// has renders as the key itself, never as an empty text.
func (r *Registry) Render(l Locale, m Message) (text, language string) {
	if c := catalogFor(l); c != nil && l.Base() != English {
		if _, ok := c[m.Key]; ok {
			text, _ = render(l.Code, c, m, l.Dir == RTL)
			return text, l.Code
		}
	}
	text, _ = render(English, catalogs[English], m, false)
	return text, English
}

// render renders m from c; a nested key that c lacks falls back to
// English. It reports false for a key that neither has.
func render(code string, c catalog, m Message, rtl bool) (string, bool) {
	t, ok := c[m.Key]
	if !ok {
		if t, ok = catalogs[English][m.Key]; !ok {
			return string(m.Key), false
		}
	}
	values := make(map[string]string, len(m.Args))
	for _, arg := range m.Args {
		switch v := arg.Value.(type) {
		case Key:
			values[arg.Name], _ = render(code, c, M(v), rtl)
		case Message:
			values[arg.Name], _ = render(code, c, v, rtl)
		default:
			literal := fmt.Sprint(v)
			if rtl {
				literal = firstStrongIsolate + literal + popDirectionalIsol
			}
			values[arg.Name] = literal
		}
	}
	return t.execute(values), true
}

// template is a parsed text: literal parts and named parameters.
type template struct {
	parts []part
}

type part struct {
	text  string
	param string
}

var paramPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parseTemplate reads a text with {name} parameters. "{{" and "}}" stand
// for literal braces.
func parseTemplate(text string) (*template, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("the text is empty")
	}
	t := &template{}
	var literal strings.Builder
	for i := 0; i < len(text); i++ {
		switch ch := text[i]; {
		case ch == '{' && i+1 < len(text) && text[i+1] == '{':
			literal.WriteByte('{')
			i++
		case ch == '}' && i+1 < len(text) && text[i+1] == '}':
			literal.WriteByte('}')
			i++
		case ch == '{':
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("a { is not closed")
			}
			name := text[i+1 : i+end]
			if !paramPattern.MatchString(name) {
				return nil, fmt.Errorf("{%s} is not a parameter: use lower-case letters, digits, and underscores", name)
			}
			if literal.Len() > 0 {
				t.parts = append(t.parts, part{text: literal.String()})
				literal.Reset()
			}
			t.parts = append(t.parts, part{param: name})
			i += end
		case ch == '}':
			return nil, fmt.Errorf("a } has no {")
		default:
			literal.WriteByte(ch)
		}
	}
	if literal.Len() > 0 {
		t.parts = append(t.parts, part{text: literal.String()})
	}
	return t, nil
}

// params returns the names of the parameters of t, each once.
func (t *template) params() map[string]bool {
	names := map[string]bool{}
	for _, p := range t.parts {
		if p.param != "" {
			names[p.param] = true
		}
	}
	return names
}

// execute fills in the parameters. A parameter without a value renders
// as nothing, never as its name in braces.
func (t *template) execute(values map[string]string) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.param == "" {
			b.WriteString(p.text)
			continue
		}
		b.WriteString(values[p.param])
	}
	return b.String()
}
