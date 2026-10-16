package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseNDJSONStreamUnwrapsLaterCallFunctionArgs(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"agent-inference","id":"step1","value":[{"type":"tool_use","id":"toolu_read","name":"callFunction","input":{}}]}`,
		`{"type":"agent-tool-result","toolCallId":"toolu_read","toolName":"callFunction","toolType":"callFunction","state":"applied","input":{"function":"Read","args":{"path":"main.go","limit":20}}}`,
		`{"type":"agent-inference","id":"step1","value":[{"type":"text","content":"done"}],"finishedAt":1}`,
	}, "\n")

	var native []AgentValueEntry
	err := parseNDJSONStream(strings.NewReader(stream), "", func(string, bool, *UsageInfo) {}, &native, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(native) != 1 {
		t.Fatalf("native tools = %#v", native)
	}
	if native[0].Name != "Read" {
		t.Fatalf("name = %q, want Read", native[0].Name)
	}
	if !strings.Contains(string(native[0].Input), `"path"`) || !strings.Contains(string(native[0].Input), "main.go") {
		t.Fatalf("input = %s", native[0].Input)
	}

	prepared := prepareToolBridgeResponse("", native)
	if len(prepared.ToolCalls) != 1 || prepared.ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("prepared = %#v", prepared.ToolCalls)
	}
	if !strings.Contains(prepared.ToolCalls[0].Function.Arguments, "main.go") {
		t.Fatalf("arguments = %s", prepared.ToolCalls[0].Function.Arguments)
	}
}

func TestPrepareToolBridgePrefersTextJSONWhenNativeCallIsEmpty(t *testing.T) {
	native := []AgentValueEntry{{
		Type:  "tool_use",
		ID:    "toolu_empty",
		Name:  "callFunction",
		Input: json.RawMessage(`{}`),
	}}
	content := `{"name":"Read","arguments":{"path":"README.md"}}`
	prepared := prepareToolBridgeResponse(content, native)
	if len(prepared.ToolCalls) != 1 || prepared.ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("prepared = %#v", prepared.ToolCalls)
	}
	if !strings.Contains(prepared.ToolCalls[0].Function.Arguments, "README.md") {
		t.Fatalf("arguments = %s", prepared.ToolCalls[0].Function.Arguments)
	}
}

func TestPrepareToolBridgeDropsInternalSearchCall(t *testing.T) {
	native := []AgentValueEntry{{
		Type:  "tool_use",
		ID:    "toolu_search",
		Name:  "callFunction",
		Input: json.RawMessage(`{"function":"connections.search.search","args":{"queries":[{"question":"news"}]}}`),
	}}
	prepared := prepareToolBridgeResponse("", native)
	if prepared.HasCalls || len(prepared.ToolCalls) != 0 {
		t.Fatalf("internal search leaked: %#v", prepared.ToolCalls)
	}
}

func TestEmitAnthropicToolUsesReachesOpenAIToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	next, logged := emitAnthropicToolUses(rec, rec, 0, []ToolCall{{
		ID:   "toolu_read",
		Type: "function",
		Function: ToolCallFunction{
			Name:      "Read",
			Arguments: `{"path":"main.go"}`,
		},
	}})
	if next != 1 || len(logged) != 1 || logged[0].Name != "Read" {
		t.Fatalf("emit = %d %#v", next, logged)
	}
	if strings.Contains(rec.Body.String(), "callFunction") {
		t.Fatalf("SSE leaked callFunction: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"Read"`) {
		t.Fatalf("SSE missing Read: %s", rec.Body.String())
	}

	openAI := httptest.NewRecorder()
	transcoder := newOpenAIChatStreamTranscoder(openAI, openAI, "chatcmpl_test", "gpt-5.6-sol", 1, false)
	for _, frame := range parseSSEFrames(t, rec.Body.String()) {
		if err := transcoder.HandleFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	body := openAI.Body.String()
	if !strings.Contains(body, `"name":"Read"`) || !strings.Contains(body, "main.go") {
		t.Fatalf("openai tool_calls = %s", body)
	}
	if strings.Contains(body, "callFunction") {
		t.Fatalf("openai leaked callFunction: %s", body)
	}
}

func TestInjectToolsIntoMessagesKeepsToolsForNonClaude(t *testing.T) {
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "Read",
			Description: "Read a file",
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}}
	got := injectToolsIntoMessages([]ChatMessage{{Role: "user", Content: "read main.go"}}, tools, "gpt-5.6-sol", nil)
	if len(got) == 0 || !strings.Contains(got[len(got)-1].Content, "Read") {
		t.Fatalf("non-claude injection dropped tools: %#v", got)
	}
}

func parseSSEFrames(t *testing.T, raw string) []anthropicSSEFrame {
	t.Helper()
	var frames []anthropicSSEFrame
	blocks := strings.Split(raw, "\n\n")
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var event string
		var data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		frames = append(frames, anthropicSSEFrame{Event: event, Data: json.RawMessage(data)})
	}
	if len(frames) == 0 {
		t.Fatalf("no SSE frames in %s", raw)
	}
	return frames
}

func TestPatchInputFillsToolArguments(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"patch-start","version":1,"data":{"s":[{}]}}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step","type":"agent-inference","value":[{"type":"tool_use","id":"tool-read","name":"callFunction","input":{}}]}}]}`,
		`{"type":"patch","v":[{"o":"p","p":"/s/1/value/0/input","v":{"function":"Read","args":{"path":"go.mod"}}}]}`,
	}, "\n")
	var native []AgentValueEntry
	if err := parseNDJSONStream(strings.NewReader(stream), "", func(string, bool, *UsageInfo) {}, &native, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(native) != 1 || native[0].Name != "Read" || !strings.Contains(string(native[0].Input), "go.mod") {
		t.Fatalf("patched tool = %#v", native)
	}
}
