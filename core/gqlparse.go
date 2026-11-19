// A GraphQL document parser: operations, variables, fields with aliases and arguments, directives,
// fragments (inline and named), and every literal kind (including block strings).
package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	gvVar = iota
	gvInt
	gvFloat
	gvString
	gvBool
	gvNull
	gvEnum
	gvList
	gvObject
)

type GValue struct {
	Kind   int
	Str    string // var name, string, enum
	Num    float64
	Bool   bool
	List   []*GValue
	Fields []GArg
}

type GArg struct {
	Name  string
	Value *GValue
}

type Directive struct {
	Name string
	Args []GArg
}

const (
	selField = iota
	selInline
	selSpread
)

type Selection struct {
	Kind          int
	Alias         string // "" when none
	Name          string // field name or spread name
	Args          []GArg
	Directives    []Directive
	Selections    []*Selection // nil for a leaf field
	TypeCondition string       // inline fragments: "" when none
	Line, Column  int
}

type VariableDefinition struct {
	Name         string
	Type         string
	DefaultValue *GValue
}

type Operation struct {
	Kind       string
	Name       string
	HasName    bool
	Variables  []VariableDefinition
	Directives []Directive
	Selections []*Selection
}

type Fragment struct {
	Name          string
	TypeCondition string
	Selections    []*Selection
}

type Document struct {
	Operations []*Operation
	Fragments  map[string]*Fragment
}

type GraphQLSyntaxError struct {
	Message      string
	Line, Column int
}

func (e *GraphQLSyntaxError) Error() string { return e.Message }

const (
	tkPunct = iota
	tkName
	tkInt
	tkFloat
	tkString
	tkEOF
)

type token struct {
	kind         int
	value        string
	line, column int
}

func isNameStart(c byte) bool { return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
func isNameChar(c byte) bool  { return isNameStart(c) || (c >= '0' && c <= '9') }
func isDigit(c byte) bool     { return c >= '0' && c <= '9' }

// col converts a byte offset within the line to a UTF-16 column, as JavaScript counts.
func col(source string, lineStart, i int) int {
	n := 0
	for _, r := range source[lineStart:i] {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n + 1
}

func syntaxErr(msg string, line, column int) *GraphQLSyntaxError {
	return &GraphQLSyntaxError{Message: msg, Line: line, Column: column}
}

func tokenize(source string) []token {
	tokens := make([]token, 0, len(source)/3+4)
	i, line, lineStart := 0, 1, 0
	n := len(source)
	for i < n {
		c := source[i]
		if c == '\n' {
			i++
			line++
			lineStart = i
			continue
		}
		if c == '\r' {
			i++
			if i < n && source[i] == '\n' {
				i++
			}
			line++
			lineStart = i
			continue
		}
		if c == ' ' || c == '\t' || c == ',' {
			i++
			continue
		}
		if strings.HasPrefix(source[i:], "\xef\xbb\xbf") {
			i += 3
			continue
		}
		if c == '#' {
			for i < n && source[i] != '\n' && source[i] != '\r' {
				i++
			}
			continue
		}
		column := col(source, lineStart, i)
		if strings.IndexByte("!$&()[]{}:=@|", c) >= 0 {
			tokens = append(tokens, token{tkPunct, string(c), line, column})
			i++
			continue
		}
		if c == '.' {
			if strings.HasPrefix(source[i:], "...") {
				tokens = append(tokens, token{tkPunct, "...", line, column})
				i += 3
				continue
			}
			panic(syntaxErr(fmt.Sprintf(`Parse error on "." (error) at [%d, %d]`, line, column), line, column))
		}
		if isNameStart(c) {
			j := i + 1
			for j < n && isNameChar(source[j]) {
				j++
			}
			tokens = append(tokens, token{tkName, source[i:j], line, column})
			i = j
			continue
		}
		if c == '-' || isDigit(c) {
			j := i
			if source[j] == '-' {
				j++
			}
			for j < n && isDigit(source[j]) {
				j++
			}
			isFloat := false
			if j < n && source[j] == '.' {
				isFloat = true
				j++
				for j < n && isDigit(source[j]) {
					j++
				}
			}
			if j < n && (source[j] == 'e' || source[j] == 'E') {
				isFloat = true
				j++
				if j < n && (source[j] == '+' || source[j] == '-') {
					j++
				}
				for j < n && isDigit(source[j]) {
					j++
				}
			}
			kind := tkInt
			if isFloat {
				kind = tkFloat
			}
			tokens = append(tokens, token{kind, source[i:j], line, column})
			i = j
			continue
		}
		if c == '"' {
			if strings.HasPrefix(source[i:], `"""`) {
				j := i + 3
				var raw strings.Builder
				for j < n && !strings.HasPrefix(source[j:], `"""`) {
					if strings.HasPrefix(source[j:], `\"""`) {
						raw.WriteString(`"""`)
						j += 4
						continue
					}
					if source[j] == '\n' {
						line++
						lineStart = j + 1
					}
					raw.WriteByte(source[j])
					j++
				}
				if j >= n {
					panic(syntaxErr("Unterminated string", line, column))
				}
				tokens = append(tokens, token{tkString, blockString(raw.String()), line, column})
				i = j + 3
				continue
			}
			j := i + 1
			var units []uint16
			var value strings.Builder
			flush := func() {
				if len(units) > 0 {
					value.WriteString(string(utf16.Decode(units)))
					units = units[:0]
				}
			}
			for j < n && source[j] != '"' {
				d := source[j]
				if d == '\n' || d == '\r' {
					panic(syntaxErr("Unterminated string", line, column))
				}
				if d == '\\' {
					var e byte
					if j+1 < n {
						e = source[j+1]
					}
					if e == 'u' {
						hex := slice(source, j+2, j+6)
						v, err := strconv.ParseUint(leadingHex(hex), 16, 32)
						if err != nil {
							v = 0
						}
						units = append(units, uint16(v))
						j += 6
						continue
					}
					flush()
					switch e {
					case '"':
						value.WriteByte('"')
					case '\\':
						value.WriteByte('\\')
					case '/':
						value.WriteByte('/')
					case 'b':
						value.WriteByte('\b')
					case 'f':
						value.WriteByte('\f')
					case 'n':
						value.WriteByte('\n')
					case 'r':
						value.WriteByte('\r')
					case 't':
						value.WriteByte('\t')
					case 0:
					default:
						value.WriteByte(e)
					}
					j += 2
					continue
				}
				flush()
				value.WriteByte(d)
				j++
			}
			flush()
			if j >= n {
				panic(syntaxErr("Unterminated string", line, column))
			}
			tokens = append(tokens, token{tkString, value.String(), line, column})
			i = j + 1
			continue
		}
		ch := source[i : i+1]
		if c >= 0x80 {
			r := []rune(source[i:])[0]
			ch = string(r)
		}
		panic(syntaxErr(fmt.Sprintf(`Parse error on "%s" (error) at [%d, %d]`, ch, line, column), line, column))
	}
	tokens = append(tokens, token{tkEOF, "", line, col(source, lineStart, minInt(i, n))})
	return tokens
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func leadingHex(s string) string {
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) {
			return s[:i]
		}
	}
	return s
}

func blockString(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	common := -1
	for k := 1; k < len(lines); k++ {
		text := lines[k]
		indent := len(text) - len(strings.TrimLeft(text, " \t"))
		if indent < len(text) && (common < 0 || indent < common) {
			common = indent
		}
	}
	if common >= 0 {
		for k := 1; k < len(lines); k++ {
			if len(lines[k]) >= common {
				lines[k] = lines[k][common:]
			} else {
				lines[k] = ""
			}
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

type gqlParser struct {
	tokens []token
	pos    int
}

func (p *gqlParser) peek() token { return p.tokens[p.pos] }

func (p *gqlParser) next() token {
	t := p.tokens[p.pos]
	if t.kind != tkEOF {
		p.pos++
	}
	return t
}

var tokenKindNames = map[int]string{tkPunct: "PUNCT", tkName: "NAME", tkInt: "INT", tkFloat: "FLOAT", tkString: "STRING"}

func (p *gqlParser) fail(t token) {
	if t.kind == tkEOF {
		panic(syntaxErr(fmt.Sprintf("Parse error on end of file (EOF) at [%d, %d]", t.line, t.column), t.line, t.column))
	}
	panic(syntaxErr(fmt.Sprintf(`Parse error on "%s" (%s) at [%d, %d]`, t.value, tokenKindNames[t.kind], t.line, t.column), t.line, t.column))
}

func (p *gqlParser) isPunct(v string) bool {
	t := p.peek()
	return t.kind == tkPunct && t.value == v
}

func (p *gqlParser) expectPunct(v string) token {
	t := p.next()
	if t.kind != tkPunct || t.value != v {
		p.fail(t)
	}
	return t
}

func (p *gqlParser) expectName() token {
	t := p.next()
	if t.kind != tkName {
		p.fail(t)
	}
	return t
}

func (p *gqlParser) parseDocument() *Document {
	doc := &Document{Fragments: map[string]*Fragment{}}
	for p.peek().kind != tkEOF {
		t := p.peek()
		if t.kind == tkPunct && t.value == "{" {
			doc.Operations = append(doc.Operations, &Operation{Kind: "query", Selections: p.parseSelectionSet()})
			continue
		}
		if t.kind == tkName && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
			p.next()
			op := &Operation{Kind: t.value}
			if p.peek().kind == tkName {
				op.Name = p.next().value
				op.HasName = true
			}
			if p.isPunct("(") {
				op.Variables = p.parseVariableDefinitions()
			}
			op.Directives = p.parseDirectives(false)
			op.Selections = p.parseSelectionSet()
			doc.Operations = append(doc.Operations, op)
			continue
		}
		if t.kind == tkName && t.value == "fragment" {
			p.next()
			name := p.expectName().value
			on := p.expectName()
			if on.value != "on" {
				p.fail(on)
			}
			typeCondition := p.expectName().value
			p.parseDirectives(false)
			doc.Fragments[name] = &Fragment{Name: name, TypeCondition: typeCondition, Selections: p.parseSelectionSet()}
			continue
		}
		p.fail(t)
	}
	if len(doc.Operations) == 0 && len(doc.Fragments) == 0 {
		p.fail(p.peek())
	}
	return doc
}

func (p *gqlParser) parseVariableDefinitions() []VariableDefinition {
	p.expectPunct("(")
	var out []VariableDefinition
	for !p.isPunct(")") {
		p.expectPunct("$")
		name := p.expectName().value
		p.expectPunct(":")
		typ := p.parseType()
		var def *GValue
		if p.isPunct("=") {
			p.next()
			def = p.parseValue(true)
		}
		p.parseDirectives(true)
		out = append(out, VariableDefinition{Name: name, Type: typ, DefaultValue: def})
	}
	p.expectPunct(")")
	return out
}

func (p *gqlParser) parseType() string {
	var typ string
	if p.isPunct("[") {
		p.next()
		typ = "[" + p.parseType() + "]"
		p.expectPunct("]")
	} else {
		typ = p.expectName().value
	}
	if p.isPunct("!") {
		p.next()
		typ += "!"
	}
	return typ
}

func (p *gqlParser) parseDirectives(isConst bool) []Directive {
	var out []Directive
	for p.isPunct("@") {
		p.next()
		name := p.expectName().value
		var args []GArg
		if p.isPunct("(") {
			args = p.parseArguments(isConst)
		}
		out = append(out, Directive{Name: name, Args: args})
	}
	return out
}

func (p *gqlParser) parseArguments(isConst bool) []GArg {
	p.expectPunct("(")
	var out []GArg
	for !p.isPunct(")") {
		name := p.expectName().value
		p.expectPunct(":")
		out = append(out, GArg{Name: name, Value: p.parseValue(isConst)})
	}
	p.expectPunct(")")
	return out
}

func (p *gqlParser) parseValue(isConst bool) *GValue {
	t := p.next()
	switch t.kind {
	case tkPunct:
		if t.value == "$" && !isConst {
			return &GValue{Kind: gvVar, Str: p.expectName().value}
		}
		if t.value == "[" {
			v := &GValue{Kind: gvList, List: []*GValue{}}
			for !p.isPunct("]") {
				v.List = append(v.List, p.parseValue(isConst))
			}
			p.next()
			return v
		}
		if t.value == "{" {
			v := &GValue{Kind: gvObject, Fields: []GArg{}}
			for !p.isPunct("}") {
				name := p.expectName().value
				p.expectPunct(":")
				v.Fields = append(v.Fields, GArg{Name: name, Value: p.parseValue(isConst)})
			}
			p.next()
			return v
		}
		p.fail(t)
	case tkInt:
		n, _ := jsParseInt(t.value)
		if f, err := strconv.ParseFloat(t.value, 64); err == nil && (f > 1<<53 || f < -(1<<53)) {
			return &GValue{Kind: gvInt, Num: f}
		}
		return &GValue{Kind: gvInt, Num: float64(n)}
	case tkFloat:
		f, err := strconv.ParseFloat(t.value, 64)
		if err != nil {
			f = 0
		}
		return &GValue{Kind: gvFloat, Num: f}
	case tkString:
		return &GValue{Kind: gvString, Str: t.value}
	case tkName:
		switch t.value {
		case "true":
			return &GValue{Kind: gvBool, Bool: true}
		case "false":
			return &GValue{Kind: gvBool, Bool: false}
		case "null":
			return &GValue{Kind: gvNull}
		}
		return &GValue{Kind: gvEnum, Str: t.value}
	}
	p.fail(t)
	return nil
}

func (p *gqlParser) parseSelectionSet() []*Selection {
	p.expectPunct("{")
	var out []*Selection
	for !p.isPunct("}") {
		out = append(out, p.parseSelection())
	}
	p.next()
	if len(out) == 0 {
		p.fail(p.tokens[p.pos-1])
	}
	return out
}

func (p *gqlParser) parseSelection() *Selection {
	if p.isPunct("...") {
		spread := p.next()
		t := p.peek()
		if t.kind == tkName && t.value == "on" {
			p.next()
			typeCondition := p.expectName().value
			directives := p.parseDirectives(false)
			return &Selection{Kind: selInline, TypeCondition: typeCondition, Directives: directives, Selections: p.parseSelectionSet()}
		}
		if t.kind == tkName {
			p.next()
			return &Selection{Kind: selSpread, Name: t.value, Directives: p.parseDirectives(false), Line: spread.line, Column: spread.column}
		}
		directives := p.parseDirectives(false)
		return &Selection{Kind: selInline, Directives: directives, Selections: p.parseSelectionSet()}
	}
	first := p.expectName()
	sel := &Selection{Kind: selField, Name: first.value, Line: first.line, Column: first.column}
	if p.isPunct(":") {
		p.next()
		sel.Alias = first.value
		sel.Name = p.expectName().value
	}
	if p.isPunct("(") {
		sel.Args = p.parseArguments(false)
	}
	sel.Directives = p.parseDirectives(false)
	if p.isPunct("{") {
		sel.Selections = p.parseSelectionSet()
	}
	return sel
}

// parseDocument parses GraphQL source; a syntax error is returned as *GraphQLSyntaxError.
func parseGraphQL(source string) (doc *Document, err error) {
	defer func() {
		if r := recover(); r != nil {
			if se, ok := r.(*GraphQLSyntaxError); ok {
				err = se
				return
			}
			panic(r)
		}
	}()
	p := &gqlParser{tokens: tokenize(source)}
	return p.parseDocument(), nil
}
