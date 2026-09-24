// Package mcp serves tools over the Model Context Protocol's stdio
// transport: newline-delimited JSON-RPC 2.0 messages on standard input and
// output. It implements only what a tools-only server needs (the
// initialize handshake with version negotiation, ping, tools/list, and
// tools/call) using the standard library alone, and knows nothing about
// SnapVault itself.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// LatestProtocolVersion is the newest MCP revision this server speaks. A
// client that asks for a version this server does not know is answered
// with it, and may then disconnect if it cannot speak it.
const LatestProtocolVersion = "2025-11-25"

// supportedVersions lists every revision this server accepts as-is. All of
// them share the lifecycle, ping, tools/list, and tools/call shapes used here.
var supportedVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", LatestProtocolVersion}

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// maxMessageBytes bounds one incoming line.
const maxMessageBytes = 16 << 20

// Tool is one callable tool. Handler receives the call's raw "arguments"
// object and returns text for the model; a returned error is reported to the
// model as a tool error, not as a protocol error.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// ReadOnly and Destructive become the readOnlyHint and destructiveHint
	// annotations clients use to decide whether to ask the user first.
	ReadOnly    bool
	Destructive bool
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server answers MCP requests for a fixed set of tools.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from in and writes responses to out, one JSON
// message per line, until in reaches end of file or ctx is cancelled.
// Requests are handled one at a time, in order.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		resp := s.handle(ctx, line)
		if resp == nil {
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// handle returns the response to one message, or nil for a notification.
func (s *Server) handle(ctx context.Context, line []byte) *response {
	if line[0] == '[' {
		return errorResponse(json.RawMessage("null"), codeInvalidRequest, "batch requests are not supported")
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(json.RawMessage("null"), codeParseError, "parse error: "+err.Error())
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		return errorResponse(id, codeInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	if len(req.ID) == 0 {
		// Notifications (initialized, cancelled, ...) need no reply. Calls
		// run synchronously, so a cancellation has nothing left to stop.
		return nil
	}

	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "ping":
		return &response{JSONRPC: "2.0", ID: req.ID, Result: struct{}{}}
	case "tools/list":
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.describeTools()}}
	case "tools/call":
		return s.callTool(ctx, req)
	}
	return errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method)
}

func (s *Server) initialize(req request) *response {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, codeInvalidParams, "invalid initialize params: "+err.Error())
		}
	}
	version := LatestProtocolVersion
	if slices.Contains(supportedVersions, params.ProtocolVersion) {
		version = params.ProtocolVersion
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		result["instructions"] = s.Instructions
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) describeTools() []map[string]any {
	tools := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"annotations": map[string]any{
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
			},
		})
	}
	return tools
}

func (s *Server) callTool(ctx context.Context, req request) *response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid tools/call params: "+err.Error())
	}
	i := slices.IndexFunc(s.Tools, func(t Tool) bool { return t.Name == params.Name })
	if i < 0 {
		return errorResponse(req.ID, codeInvalidParams, fmt.Sprintf("unknown tool: %s", params.Name))
	}
	args := params.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	text, err := s.Tools[i].Handler(ctx, args)
	isError := err != nil
	if isError {
		text = err.Error()
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}}
}

func errorResponse(id json.RawMessage, code int, message string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}
