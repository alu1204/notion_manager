package notionapi

import (
	"bytes"
	"encoding/json"
)

// UnwrapRecord peels Notion's v3 record wrapper.
//
// Current responses use either {"value": {...payload}} or
// {"value": {"value": {...payload}, "role": "editor"}}, and fanout cells
// sometimes add one more role/value layer. The payload is the first object
// that is not itself a role/value wrapper.
func UnwrapRecord(raw json.RawMessage) map[string]interface{} {
	current := bytes.TrimSpace(raw)
	for i := 0; i < 6 && len(current) > 0 && current[0] == '{'; i++ {
		var probe struct {
			Value json.RawMessage `json:"value"`
			Role  string          `json:"role"`
		}
		if json.Unmarshal(current, &probe) != nil || len(bytes.TrimSpace(probe.Value)) == 0 {
			break
		}
		inner := bytes.TrimSpace(probe.Value)
		if inner[0] != '{' {
			break
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(inner, &fields) != nil {
			break
		}
		_, nested := fields["value"]
		_, role := fields["role"]
		if nested || (role && nested) {
			current = inner
			continue
		}
		// A role wrapper whose value is the payload, or a plain value wrapper.
		if probe.Role != "" || nested {
			current = inner
			continue
		}
		var data map[string]interface{}
		if json.Unmarshal(inner, &data) == nil {
			return data
		}
		break
	}
	var data map[string]interface{}
	if json.Unmarshal(current, &data) != nil {
		return nil
	}
	if _, wrapped := data["value"]; wrapped && len(data) <= 2 {
		return nil
	}
	return data
}

func StringField(data map[string]interface{}, keys ...string) string {
	if data == nil {
		return ""
	}
	for _, key := range keys {
		switch v := data[key].(type) {
		case string:
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func BoolField(data map[string]interface{}, keys ...string) (bool, bool) {
	if data == nil {
		return false, false
	}
	for _, key := range keys {
		switch v := data[key].(type) {
		case bool:
			return v, true
		}
	}
	return false, false
}
