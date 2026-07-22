package store

import (
	"encoding/json"
	"strings"
)

type SchemaFindingKind string

const (
	FindingOneOf           SchemaFindingKind = "oneOf"
	FindingAnyOf           SchemaFindingKind = "anyOf"
	FindingAllOf           SchemaFindingKind = "allOf"
	FindingNot             SchemaFindingKind = "not"
	FindingRef             SchemaFindingKind = "ref"
	FindingExternalRef     SchemaFindingKind = "externalRef"
	FindingUntypedProperty SchemaFindingKind = "untypedProperty"
)

type SchemaFinding struct {
	Kind SchemaFindingKind
}

func analyzeSchema(raw json.RawMessage) []SchemaFinding {
	if len(raw) == 0 {
		return nil
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}

	var findings []SchemaFinding
	walkSchema(v, &findings)
	return findings
}

func walkSchema(v any, findings *[]SchemaFinding) {
	node, ok := v.(map[string]any)
	if !ok {
		return
	}

	if _, ok := node["oneOf"]; ok {
		*findings = append(*findings, SchemaFinding{Kind: FindingOneOf})
	}
	if _, ok := node["anyOf"]; ok {
		*findings = append(*findings, SchemaFinding{Kind: FindingAnyOf})
	}
	if _, ok := node["allOf"]; ok {
		*findings = append(*findings, SchemaFinding{Kind: FindingAllOf})
	}
	if _, ok := node["not"]; ok {
		*findings = append(*findings, SchemaFinding{Kind: FindingNot})
	}
	if ref, ok := node["$ref"].(string); ok {
		kind := FindingExternalRef
		if strings.HasPrefix(ref, "#") {
			kind = FindingRef
		}
		*findings = append(*findings, SchemaFinding{Kind: kind})
	}

	if props, ok := node["properties"].(map[string]any); ok {
		for _, child := range props {
			if schema, ok := child.(map[string]any); ok {
				if _, hasType := schema["type"]; !hasType {
					*findings = append(*findings, SchemaFinding{Kind: FindingUntypedProperty})
				}
				walkSchema(schema, findings)
			}
		}
	}

	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if arr, ok := node[key].([]any); ok {
			for _, child := range arr {
				walkSchema(child, findings)
			}
		}
	}

	for _, key := range []string{"not", "items"} {
		if child, ok := node[key]; ok {
			walkSchema(child, findings)
		}
	}
}
