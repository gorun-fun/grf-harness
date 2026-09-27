// schema.go workflow 子代理结构化输出（opts.schema）的校验器：用受限
// 键集校验 agent() 带回的 JSON 值，键集限定 type/properties/required/
// additionalProperties/items/enum/const/oneOf（自 workflow.go 拆出，
// 提取候选 JSON 的部分在 jsonx.go）。
package agent

import (
	"encoding/json"
	"fmt"
)

// ValidateSchema 用受限键集校验值：type/properties/required/
// additionalProperties/items/enum/const/oneOf。
func ValidateSchema(v any, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	for k := range schema {
		switch k {
		case "type", "properties", "required", "additionalProperties", "items", "enum", "const", "oneOf", "description":
		default:
			return fmt.Errorf("schema key %q is not allowed (only type/properties/required/additionalProperties/items/enum/const/oneOf)", k)
		}
	}
	if c, ok := schema["const"]; ok {
		if !jsonEqual(v, c) {
			return fmt.Errorf("value does not equal const")
		}
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, item := range e {
			if jsonEqual(v, item) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("value not in enum")
		}
	}
	if t, ok := schema["type"].(string); ok {
		if err := checkType(v, t); err != nil {
			return err
		}
	}
	if oneOf, ok := schema["oneOf"].([]any); ok {
		matched := 0
		for _, sub := range oneOf {
			if sm, ok := sub.(map[string]any); ok {
				if ValidateSchema(v, sm) == nil {
					matched++
				}
			}
		}
		if matched != 1 {
			return fmt.Errorf("value matches %d oneOf branches (need exactly 1)", matched)
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("properties requires an object value")
		}
		for key, sub := range props {
			if sm, ok := sub.(map[string]any); ok {
				if err := ValidateSchema(obj[key], sm); err != nil {
					return fmt.Errorf("property %q: %w", key, err)
				}
			}
		}
	}
	if req, ok := schema["required"].([]any); ok {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("required requires an object value")
		}
		for _, key := range req {
			ks, _ := key.(string)
			if _, ok := obj[ks]; !ok {
				return fmt.Errorf("missing required property %q", ks)
			}
		}
	}
	if ap, ok := schema["additionalProperties"]; ok {
		if apBool, isBool := ap.(bool); isBool && !apBool {
			obj, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("additionalProperties requires an object value")
			}
			props := map[string]bool{}
			if p, ok := schema["properties"].(map[string]any); ok {
				for k := range p {
					props[k] = true
				}
			}
			for k := range obj {
				if !props[k] {
					return fmt.Errorf("additional property %q is not allowed", k)
				}
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("items requires an array value")
		}
		for i, e := range arr {
			if err := ValidateSchema(e, items); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	}
	return nil
}

// checkType 校验 JSON Schema type 单值（object/array/string/number/
// integer/boolean/null）。
func checkType(v any, t string) error {
	switch t {
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("expected object, got %T", v)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("expected array, got %T", v)
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
	case "number":
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("expected number, got %T", v)
		}
		_ = n
	case "integer":
		n, ok := v.(float64)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("expected integer, got %T", v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", v)
		}
	case "null":
		if v != nil {
			return fmt.Errorf("expected null, got %T", v)
		}
	default:
		return fmt.Errorf("unsupported schema type %q", t)
	}
	return nil
}

// jsonEqual 按 JSON 序列化文本比较两个值（enum/const 匹配用）。
func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
