package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The capture format is a contract other tools build on, and docs/CAPTURE_FORMAT.md
// with docs/capture.schema.json is where it is written down. These tests hold
// both to the code, so a field added to an envelope without a word in either
// fails here rather than in somebody else's importer.

var (
	captureDocPath    = filepath.Join("..", "..", "docs", "CAPTURE_FORMAT.md")
	captureSchemaPath = filepath.Join("..", "..", "docs", "capture.schema.json")
)

// wireField is one struct field as it appears in a capture.
type wireField struct {
	name      string
	omitempty bool
	typ       reflect.Type
}

func wireFields(t *testing.T, typ reflect.Type) []wireField {
	t.Helper()
	var fields []wireField
	for f := range typ.Fields() {
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no json name, so the format cannot say what it is called", typ.Name(), f.Name)
		}
		name, opts, _ := strings.Cut(tag, ",")
		fields = append(fields, wireField{name: name, omitempty: strings.Contains(opts, "omitempty"), typ: f.Type})
	}
	return fields
}

// schemaDefs names the $defs entry that describes each nested struct.
var schemaDefs = map[reflect.Type]string{
	reflect.TypeFor[SessionMeta]():      "sessionMeta",
	reflect.TypeFor[TransportHeaders](): "transportHeaders",
	reflect.TypeFor[MCPParamHeader]():   "mcpParamHeader",
	reflect.TypeFor[TraceContext]():     "traceContext",
}

func loadCaptureSchema(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(captureSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatalf("%s is not valid JSON: %v", captureSchemaPath, err)
	}
	return schema
}

func schemaDef(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	defs, _ := schema["$defs"].(map[string]any)
	def, ok := defs[name].(map[string]any)
	if !ok {
		t.Fatalf("the schema has no $defs/%s", name)
	}
	return def
}

func TestCaptureSchemaDescribesEveryField(t *testing.T) {
	schema := loadCaptureSchema(t)
	checkSchemaObject(t, schema, "envelope", reflect.TypeFor[Envelope](), schema)
	for typ, def := range schemaDefs {
		checkSchemaObject(t, schema, def, typ, schemaDef(t, schema, def))
	}
}

// checkSchemaObject holds one schema object to one struct. Every field must be a
// property of the right type, no property may name a field that is not there,
// and the required list must be exactly the fields written even when empty.
func checkSchemaObject(t *testing.T, schema map[string]any, label string, typ reflect.Type, node map[string]any) {
	t.Helper()
	props, _ := node["properties"].(map[string]any)
	var required []string
	for _, r := range anySlice(node["required"]) {
		required = append(required, r.(string))
	}
	var wantRequired []string
	seen := make(map[string]bool)
	for _, f := range wireFields(t, typ) {
		seen[f.name] = true
		if !f.omitempty {
			wantRequired = append(wantRequired, f.name)
		}
		prop, ok := props[f.name].(map[string]any)
		if !ok {
			t.Errorf("%s: field %q is written but the schema has no property for it", label, f.name)
			continue
		}
		checkSchemaType(t, schema, label+"."+f.name, f.typ, prop)
	}
	for name := range props {
		if !seen[name] {
			t.Errorf("%s: the schema describes %q, which %s does not write", label, name, typ.Name())
		}
	}
	slices.Sort(required)
	slices.Sort(wantRequired)
	if !slices.Equal(required, wantRequired) {
		t.Errorf("%s: schema requires %v, but the fields written on every envelope are %v", label, required, wantRequired)
	}
}

func checkSchemaType(t *testing.T, schema map[string]any, label string, typ reflect.Type, prop map[string]any) {
	t.Helper()
	if ref, ok := prop["$ref"].(string); ok {
		elem := typ
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if want := "#/$defs/" + schemaDefs[elem]; ref != want {
			t.Errorf("%s: refers to %s, want %s", label, ref, want)
		}
		return
	}
	if _, ok := prop["enum"]; ok {
		if typ.Kind() != reflect.String {
			t.Errorf("%s: an enum in the schema, but a %s in the code", label, typ)
		}
		return
	}
	got := prop["type"]
	switch {
	case typ == reflect.TypeFor[json.RawMessage]():
		if got != nil {
			t.Errorf("%s: carries any JSON value, so the schema must not give it a type, got %v", label, got)
		}
	case typ == reflect.TypeFor[time.Time]():
		if got != "string" || prop["format"] != "date-time" {
			t.Errorf("%s: want a date-time string, got type %v format %v", label, got, prop["format"])
		}
	case typ.Kind() == reflect.String:
		expectType(t, label, got, "string")
	case typ.Kind() == reflect.Bool:
		expectType(t, label, got, "boolean")
	case typ.Kind() == reflect.Int || typ.Kind() == reflect.Uint64:
		expectType(t, label, got, "integer")
	case typ.Kind() == reflect.Slice:
		// A nil slice without omitempty is written as null, so the schema has to
		// allow it, and only then.
		if wantsNull := typ.Elem().Kind() == reflect.String; wantsNull {
			if !slices.Equal(anySlice(got), []any{"array", "null"}) {
				t.Errorf("%s: written as null when empty, want type [array null], got %v", label, got)
			}
		} else {
			expectType(t, label, got, "array")
		}
		items, _ := prop["items"].(map[string]any)
		if items == nil {
			t.Errorf("%s: an array with no items schema", label)
			return
		}
		checkSchemaType(t, schema, label+"[]", typ.Elem(), items)
	default:
		t.Errorf("%s: %s has no rule here yet, so say what JSON it becomes", label, typ)
	}
}

func expectType(t *testing.T, label string, got any, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: want type %q, got %v", label, want, got)
	}
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// TestCaptureSchemaEnumsMatchTheCode reads the values from frame.go itself, so a
// direction or a transport added there cannot be missed by a list kept here.
func TestCaptureSchemaEnumsMatchTheCode(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "frame.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var directions, transports []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, _ := strconv.Unquote(lit.Value)
				if ident, ok := vs.Type.(*ast.Ident); ok && ident.Name == "Direction" {
					directions = append(directions, value)
				} else if strings.HasPrefix(name.Name, "Transport") {
					transports = append(transports, value)
				}
			}
		}
	}

	schema := loadCaptureSchema(t)
	props := schema["properties"].(map[string]any)
	for field, want := range map[string][]string{"direction": directions, "transport": transports} {
		var got []string
		for _, v := range anySlice(props[field].(map[string]any)["enum"]) {
			got = append(got, v.(string))
		}
		slices.Sort(got)
		slices.Sort(want)
		if len(want) == 0 || !slices.Equal(got, want) {
			t.Errorf("%s: the schema lists %v, frame.go defines %v", field, got, want)
		}
	}
}

// TestCaptureFormatDocNamesEveryField holds the page's four tables to the four
// structs, in the order the page presents them.
func TestCaptureFormatDocNamesEveryField(t *testing.T) {
	doc, err := os.ReadFile(captureDocPath)
	if err != nil {
		t.Fatal(err)
	}
	tables := docFieldTables(string(doc))
	order := []reflect.Type{
		reflect.TypeFor[Envelope](),
		reflect.TypeFor[MCPParamHeader](),
		reflect.TypeFor[TransportHeaders](),
		reflect.TypeFor[TraceContext](),
		reflect.TypeFor[SessionMeta](),
	}
	if len(tables) != len(order) {
		t.Fatalf("the page has %d field tables, want %d (envelope, mcp_param_headers, transport_headers, trace_context, meta raw)", len(tables), len(order))
	}
	for i, typ := range order {
		var want []string
		for _, f := range wireFields(t, typ) {
			want = append(want, f.name)
		}
		if !slices.Equal(tables[i], want) {
			t.Errorf("table %d documents %v, %s writes %v in that order", i+1, tables[i], typ.Name(), want)
		}
	}
}

// docFieldTables returns the first column of every markdown table whose rows
// name fields in backticks, one slice per table.
func docFieldTables(doc string) [][]string {
	var tables [][]string
	var current []string
	inFence := false
	for line := range strings.Lines(doc) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "| `") {
			cell, _, _ := strings.Cut(strings.TrimPrefix(line, "| "), " |")
			current = append(current, strings.Trim(cell, "`"))
			continue
		}
		if !inFence && strings.HasPrefix(line, "|") {
			continue // the header and the separator row
		}
		if len(current) > 0 {
			tables = append(tables, current)
			current = nil
		}
	}
	if len(current) > 0 {
		tables = append(tables, current)
	}
	return tables
}

// TestCaptureFormatExamplesAreWhatTheSinkWrites decodes every example line
// strictly and writes it back through the file sink. A line that comes back
// different is not something mcpsnoop would have written.
func TestCaptureFormatExamplesAreWhatTheSinkWrites(t *testing.T) {
	doc, err := os.ReadFile(captureDocPath)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	inBlock := false
	for line := range strings.Lines(string(doc)) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "```jsonl":
			inBlock = true
		case line == "```":
			inBlock = false
		case inBlock:
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatal("the page has no jsonl examples")
	}

	lastSeq := make(map[string]uint64)
	for _, line := range lines {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			t.Fatalf("example does not decode as an envelope: %v\n%s", err, line)
		}
		if env.Seq != lastSeq[env.SessionID]+1 {
			t.Errorf("example %s has seq %d after %d, and an unbroken session counts up by one", env.SessionID, env.Seq, lastSeq[env.SessionID])
		}
		lastSeq[env.SessionID] = env.Seq
		if env.Direction == DirectionMeta {
			meta := json.NewDecoder(bytes.NewReader(env.Raw))
			meta.DisallowUnknownFields()
			if err := meta.Decode(new(SessionMeta)); err != nil {
				t.Errorf("example meta raw is not a SessionMeta: %v", err)
			}
		}

		var buf bytes.Buffer
		sink := NewAsyncSink(&buf, 1)
		sink.Emit(env)
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSuffix(buf.String(), "\n"); got != line {
			t.Errorf("the sink writes this example differently\n got %s\nwant %s", got, line)
		}
	}
}

// TestCaptureRawKeepsWhatThePeerSent pins what the page promises about raw. Only
// insignificant whitespace goes, and U+2028 and U+2029 may come back escaped.
// Everything else the peer chose, including bytes that are not UTF-8, stays.
func TestCaptureRawKeepsWhatThePeerSent(t *testing.T) {
	sent := "{ \"jsonrpc\" : \"2.0\", \"id\" : 1, \"method\" : \"m\", \"params\" : { \"n\" : 1.0, \"e\" : 1e2, " +
		"\"dup\" : 1, \"dup\" : 2, \"esc\" : \"caf\\u00e9\\/\", \"html\" : \"<b>&</b>\", " +
		"\"sep\" : \"a\u2028b\u2029c\", \"bad\" : \"x\xffy\" } }"

	var buf bytes.Buffer
	sink := NewAsyncSink(&buf, 1)
	sink.Emit(Envelope{
		SessionID: "s", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: ClientToServer, Transport: TransportStdio, Raw: json.RawMessage(sent),
	})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	start := strings.Index(line, `"raw":`)
	if start < 0 {
		t.Fatalf("no raw in %q", line)
	}
	raw := strings.TrimSuffix(strings.TrimSuffix(line[start+len(`"raw":`):], "\n"), "}")

	want := strings.NewReplacer(" ", "").Replace(sent)
	for _, sep := range []struct{ literal, escaped string }{
		{"\u2028", `\u2028`}, {"\u2029", `\u2029`},
	} {
		// Either spelling is the same string once decoded. The test asks only
		// that nothing else moved.
		raw = strings.ReplaceAll(raw, sep.escaped, sep.literal)
	}
	if raw != want {
		t.Fatalf("raw changed more than whitespace\n got %q\nwant %q", raw, want)
	}

	// And the line still reads back, which is what a reader of the file needs.
	sc := bufio.NewScanner(strings.NewReader(line))
	sc.Scan()
	var env Envelope
	if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
		t.Fatalf("the written line does not decode: %v", err)
	}
}
