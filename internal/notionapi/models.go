package notionapi

import "encoding/json"

// Model is one selectable Notion AI model.
type Model struct {
	ID       string
	Name     string
	Family   string
	Disabled bool
}

// ParseAvailableModels reads Notion's getAvailableModels payload.
//
// The live response is still usually {"models":[{"model","modelMessage","isDisabled"}]}.
// Newer cells also use id/displayName/disabled, and some wrappers put the
// array under availableModels or data. Disabled entries are omitted.
func ParseAvailableModels(body []byte) ([]Model, error) {
	body = bytesTrim(body)
	if len(body) == 0 {
		return nil, nil
	}
	rawModels, err := modelArray(body)
	if err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(rawModels))
	for _, raw := range rawModels {
		model, ok := modelFromRaw(raw)
		if !ok || model.Disabled {
			continue
		}
		models = append(models, model)
	}
	return models, nil
}

func modelArray(body []byte) ([]json.RawMessage, error) {
	if body[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	for _, key := range []string{"models", "availableModels", "available_models", "data", "result"} {
		raw, ok := root[key]
		if !ok {
			continue
		}
		raw = bytesTrim(raw)
		if len(raw) == 0 {
			continue
		}
		if raw[0] == '[' {
			var arr []json.RawMessage
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, err
			}
			return arr, nil
		}
		if raw[0] == '{' {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) != nil {
				continue
			}
			for _, nestedKey := range []string{"models", "availableModels", "data"} {
				if inner, ok := nested[nestedKey]; ok && len(bytesTrim(inner)) > 0 && bytesTrim(inner)[0] == '[' {
					var arr []json.RawMessage
					if err := json.Unmarshal(inner, &arr); err != nil {
						return nil, err
					}
					return arr, nil
				}
			}
		}
	}
	return nil, nil
}

func modelFromRaw(raw json.RawMessage) (Model, bool) {
	var data map[string]interface{}
	if json.Unmarshal(raw, &data) != nil || len(data) == 0 {
		return Model{}, false
	}
	if inner, ok := data["value"].(map[string]interface{}); ok {
		data = inner
		if deeper, ok := data["value"].(map[string]interface{}); ok {
			data = deeper
		}
	}
	id := StringField(data, "model", "id", "modelId", "model_id")
	if id == "" {
		return Model{}, false
	}
	name := StringField(data, "modelMessage", "displayName", "display_name", "name", "title", "label")
	if name == "" {
		name = id
	}
	disabled, _ := BoolField(data, "isDisabled", "disabled", "is_disabled")
	return Model{
		ID:       id,
		Name:     name,
		Family:   StringField(data, "modelFamily", "family", "provider"),
		Disabled: disabled,
	}, true
}

func bytesTrim(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(trimSpace(raw))
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}
