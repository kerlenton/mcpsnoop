package store

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const (
	mcpParamHeaderPrefix = "Mcp-Param-"
	maxSafeMCPInteger    = int64(1<<53 - 1)
)

type paramHeaderSchema struct {
	Type       string                       `json:"type"`
	Header     json.RawMessage              `json:"x-mcp-header"`
	Properties map[string]paramHeaderSchema `json:"properties"`
}

type paramHeaderBinding struct {
	path   []string
	header string
	typ    string
}

func sortedMCPParamHeaders(headers []proxy.MCPParamHeader) []proxy.MCPParamHeader {
	out := slices.Clone(headers)
	slices.SortStableFunc(out, func(a, b proxy.MCPParamHeader) int {
		if n := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func mcpParamHeaderWarnings(sess *session, msg proxy.RPCMessage, headers []proxy.MCPParamHeader) []string {
	var params struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(msg.Params, &params) != nil || params.Name == "" {
		return nil
	}
	definition, ok := sess.toolDefinitions[params.Name]
	if !ok {
		return nil
	}
	bindings := definition.paramHeaders
	if len(bindings) == 0 {
		return nil
	}

	values := make(map[string]string, len(headers))
	for _, header := range headers {
		key := strings.ToLower(header.Name)
		if _, exists := values[key]; !exists {
			values[key] = header.Value
		}
	}

	var warnings []string
	for _, binding := range bindings {
		fullName := mcpParamHeaderPrefix + binding.header
		headerValue, present := values[strings.ToLower(fullName)]
		raw, exists := lookupParamArgument(params.Arguments, binding.path)
		path := strings.Join(binding.path, ".")
		if !exists || string(raw) == "null" {
			if present {
				warnings = append(warnings, "routing header "+fullName+
					" is present but body parameter "+strconv.Quote(path)+" is absent or null")
			}
			continue
		}
		if !present {
			warnings = append(warnings, "required routing header "+fullName+
				" is missing for body parameter "+strconv.Quote(path))
			continue
		}

		decoded, ok := decodeMCPParamHeaderValue(headerValue)
		if !ok {
			warnings = append(warnings, "routing header "+fullName+" has invalid Base64 encoding")
			continue
		}
		bodyValue, ok := mcpParamPrimitive(raw, binding.typ)
		if !ok {
			warnings = append(warnings, "routing header "+fullName+
				" cannot be compared because body parameter "+strconv.Quote(path)+
				" is not a valid "+binding.typ)
			continue
		}
		if !mcpParamValuesEqual(decoded, bodyValue) {
			warnings = append(warnings, "routing header "+fullName+" "+strconv.Quote(decoded)+
				" disagrees with body parameter "+strconv.Quote(path)+" "+strconv.Quote(mcpParamString(bodyValue)))
		}
	}
	return warnings
}

func mcpParamHeaderBindings(schema json.RawMessage) ([]paramHeaderBinding, bool) {
	var root paramHeaderSchema
	if json.Unmarshal(schema, &root) != nil {
		return nil, false
	}
	seen := make(map[string]struct{})
	bindings, ok := collectMCPParamHeaderBindings(root.Properties, nil, seen, nil)
	if !ok {
		return nil, false
	}
	slices.SortFunc(bindings, func(a, b paramHeaderBinding) int {
		if n := strings.Compare(strings.ToLower(a.header), strings.ToLower(b.header)); n != 0 {
			return n
		}
		return strings.Compare(strings.Join(a.path, "."), strings.Join(b.path, "."))
	})
	return bindings, true
}

func collectMCPParamHeaderBindings(properties map[string]paramHeaderSchema, prefix []string, seen map[string]struct{}, out []paramHeaderBinding) ([]paramHeaderBinding, bool) {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		property := properties[name]
		path := append(slices.Clone(prefix), name)
		if len(property.Header) != 0 {
			var header string
			if json.Unmarshal(property.Header, &header) != nil || header == "" ||
				!validMCPParamHeaderName(header) ||
				(property.Type != "string" && property.Type != "integer" && property.Type != "boolean") {
				return nil, false
			}
			key := strings.ToLower(header)
			if _, duplicate := seen[key]; duplicate {
				return nil, false
			}
			seen[key] = struct{}{}
			out = append(out, paramHeaderBinding{path: path, header: header, typ: property.Type})
		}
		var ok bool
		out, ok = collectMCPParamHeaderBindings(property.Properties, path, seen, out)
		if !ok {
			return nil, false
		}
	}
	return out, true
}

func validMCPParamHeaderName(name string) bool {
	for _, c := range name {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return name != ""
}

func lookupParamArgument(arguments map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	if len(path) == 0 {
		return nil, false
	}
	current, ok := arguments[path[0]]
	for _, part := range path[1:] {
		if !ok {
			return nil, false
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(current, &object) != nil {
			return nil, false
		}
		current, ok = object[part]
	}
	return current, ok
}

func decodeMCPParamHeaderValue(value string) (string, bool) {
	inner, encoded := strings.CutPrefix(value, base64SentinelPrefix)
	if !encoded {
		return value, true
	}
	inner, encoded = strings.CutSuffix(inner, base64SentinelSuffix)
	if !encoded {
		return value, true
	}
	if decoded, err := base64.StdEncoding.DecodeString(inner); err == nil {
		return string(decoded), true
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(inner); err == nil {
		return string(decoded), true
	}
	return "", false
}

func mcpParamPrimitive(raw json.RawMessage, typ string) (any, bool) {
	switch typ {
	case "string":
		var value string
		return value, json.Unmarshal(raw, &value) == nil
	case "boolean":
		var value bool
		return value, json.Unmarshal(raw, &value) == nil
	case "integer":
		value, ok := parseSafeMCPInteger(string(raw))
		if !ok {
			return nil, false
		}
		return value, true
	default:
		return nil, false
	}
}

func mcpParamValuesEqual(header string, body any) bool {
	if integer, ok := body.(int64); ok {
		value, ok := parseSafeMCPInteger(header)
		return ok && value == integer
	}
	return header == mcpParamString(body)
}

func parseSafeMCPInteger(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	exact, ok := new(big.Rat).SetString(value)
	if !ok || !exact.IsInt() || !exact.Num().IsInt64() {
		return 0, false
	}
	integer := exact.Num().Int64()
	if integer < -maxSafeMCPInteger || integer > maxSafeMCPInteger {
		return 0, false
	}
	return integer, true
}

func mcpParamString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case int64:
		return strconv.FormatInt(value, 10)
	default:
		return ""
	}
}
