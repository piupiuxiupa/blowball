//go:build ignore

// stdio_server is a minimal MCP stdio server used by mcpclient tests.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *errorObj       `json:"error,omitempty"`
}

type errorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		resp := response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = mustMarshal(map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]string{"name": "test", "version": "0.1.0"},
			})
		case "tools/list":
			resp.Result = mustMarshal(map[string]any{
				"tools": []tool{{Name: "add", Description: "adds", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			})
		case "tools/call":
			resp.Result = mustMarshal(map[string]any{
				"content": []map[string]string{{"type": "text", "text": "done"}},
			})
		default:
			resp.Error = &errorObj{Code: -32601, Message: "method not found"}
		}
		fmt.Println(string(mustMarshal(resp)))
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
