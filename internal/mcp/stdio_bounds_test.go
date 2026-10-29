package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestStdioReadBufferTenMiBAndMalformedLines(t *testing.T) {
	large := strings.Repeat("x", 9<<20)
	answer, _ := json.Marshal(wire{JSONRPC: "2.0", ID: json.RawMessage(`1`), Result: mustJSON(map[string]string{"text": large})})
	ch := make(chan response, 1)
	c := &Client{pending: map[string]chan response{"1": ch}, done: make(chan struct{}), warnings: make(chan string, 3)}
	input := io.MultiReader(strings.NewReader("debug banner\n\n{bad json\n{\"jsonrpc\":\"1.0\",\"id\":1,\"result\":{}}\n"), bytes.NewReader(answer), strings.NewReader("\n"))
	c.readLoop(input)
	result := <-ch
	if result.Error != nil || !bytes.Contains(result.Result, []byte(large)) {
		t.Fatalf("9MiB stdio frame failed: %v", result.Error)
	}
	if len(c.warnings) != 1 {
		t.Fatalf("schema warnings %d; debug lines must be ignored", len(c.warnings))
	}
}

func TestStdioReadBufferRejectsUnreadBytesOverTenMiB(t *testing.T) {
	c := &Client{pending: map[string]chan response{}, done: make(chan struct{})}
	c.readLoop(strings.NewReader(strings.Repeat("x", stdioReadBufferBytes+1)))
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "10485760") {
		t.Fatalf("overflow error %v", err)
	}
}
