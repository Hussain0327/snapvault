package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testServer() *Server {
	return &Server{
		Name:         "snapvault",
		Version:      "test",
		Instructions: "use checkpoints",
		Tools: []Tool{
			{
				Name:        "echo",
				Description: "echoes its text argument",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				ReadOnly:    true,
				Handler: func(_ context.Context, args json.RawMessage) (string, error) {
					var in struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(args, &in); err != nil {
						return "", err
					}
					return in.Text, nil
				},
			},
			{
				Name:        "fail",
				Description: "always fails",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Destructive: true,
				Handler: func(context.Context, json.RawMessage) (string, error) {
					return "", errors.New("nope")
				},
			},
		},
	}
}

// run feeds one JSON-RPC message per line and returns each response line
// decoded.
func run(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	var responses []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response %q is not JSON: %v", line, err)
		}
		responses = append(responses, m)
	}
	return responses
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", m)
	}
	return r
}

func errorCode(t *testing.T, m map[string]any) float64 {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", m)
	}
	return e["code"].(float64)
}

func TestInitializeEchoesASupportedVersion(t *testing.T) {
	got := run(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	r := result(t, got[0])
	if r["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's supported version echoed", r["protocolVersion"])
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities = %v, want tools", r["capabilities"])
	}
	if r["serverInfo"].(map[string]any)["name"] != "snapvault" || r["instructions"] != "use checkpoints" {
		t.Errorf("serverInfo/instructions = %v / %v", r["serverInfo"], r["instructions"])
	}
}

func TestInitializeAnswersAnUnknownVersionWithTheLatest(t *testing.T) {
	got := run(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := result(t, got[0])["protocolVersion"]; v != LatestProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", v, LatestProtocolVersion)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	got := run(t, testServer(),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`,
		`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if len(got) != 1 || got[0]["id"] != "p" {
		t.Fatalf("responses = %v, want only the ping reply", got)
	}
	if len(result(t, got[0])) != 0 {
		t.Errorf("ping result = %v, want {}", got[0]["result"])
	}
}

func TestToolsListDescribesEveryTool(t *testing.T) {
	got := run(t, testServer(), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := result(t, got[0])["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v, want 2", tools)
	}
	echo := tools[0].(map[string]any)
	if echo["name"] != "echo" || echo["inputSchema"].(map[string]any)["type"] != "object" {
		t.Errorf("echo tool = %v", echo)
	}
	if echo["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Errorf("echo annotations = %v, want readOnlyHint", echo["annotations"])
	}
	if tools[1].(map[string]any)["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Errorf("fail annotations = %v, want destructiveHint", tools[1])
	}
}

func TestToolsCallReturnsTextOrAToolError(t *testing.T) {
	got := run(t, testServer(),
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"missing"}}`)

	ok := result(t, got[0])
	content := ok["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != "hi" || ok["isError"] != false {
		t.Errorf("echo result = %v", ok)
	}
	failed := result(t, got[1])
	if failed["isError"] != true || !strings.Contains(failed["content"].([]any)[0].(map[string]any)["text"].(string), "nope") {
		t.Errorf("fail result = %v, want isError with the message", failed)
	}
	if code := errorCode(t, got[2]); code != codeInvalidParams {
		t.Errorf("unknown tool code = %v, want %d", code, codeInvalidParams)
	}
}

func TestProtocolErrors(t *testing.T) {
	got := run(t, testServer(),
		`not json`,
		`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`,
		`[{"jsonrpc":"2.0","id":7,"method":"ping"}]`)
	if len(got) != 3 {
		t.Fatalf("responses = %v, want 3", got)
	}
	if code := errorCode(t, got[0]); code != codeParseError {
		t.Errorf("parse error code = %v", code)
	}
	if code := errorCode(t, got[1]); code != codeMethodNotFound {
		t.Errorf("unknown method code = %v", code)
	}
	if code := errorCode(t, got[2]); code != codeInvalidRequest {
		t.Errorf("batch code = %v", code)
	}
}
