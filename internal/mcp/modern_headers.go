package mcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func encodeHeaderValue(value string) string {
	encode := value == "" || strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") || strings.TrimSpace(value) != value
	for _, r := range value {
		if r != '\t' && (r < 0x20 || r > 0x7e) {
			encode = true
		}
	}
	if encode {
		return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	}
	return value
}

func applyModernHeaders(req *http.Request, body []byte) {
	var message wire
	if json.Unmarshal(body, &message) != nil || len(message.ID) == 0 || message.Method == "" {
		return
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(message.Params, &params) != nil {
		return
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil {
		return
	}
	var version string
	if json.Unmarshal(meta[protocolMetaKey], &version) != nil || version == "" {
		return
	}
	req.Header.Set("MCP-Protocol-Version", version)
	req.Header.Set("Mcp-Method", message.Method)
	field := ""
	switch message.Method {
	case "tools/call", "prompts/get":
		field = "name"
	case "resources/read":
		field = "uri"
	case "tasks/get", "tasks/update", "tasks/cancel":
		field = "taskId"
	}
	var name string
	if field != "" && json.Unmarshal(params[field], &name) == nil {
		req.Header.Set("Mcp-Name", encodeHeaderValue(name))
	}
}

func matchingHTTPError(err error, request wire) *RPCError {
	var status *HTTPStatusError
	if !errors.As(err, &status) || status.StatusCode != 400 {
		return nil
	}
	var response wire
	if json.Unmarshal(status.Body, &response) == nil && response.JSONRPC == "2.0" && response.Error != nil && responseKey(response.ID) == responseKey(request.ID) {
		return response.Error
	}
	return nil
}
