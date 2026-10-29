package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"dsgo/internal/state"
)

const protocolMetaKey = "io.modelcontextprotocol/protocolVersion"
const clientInfoMetaKey = "io.modelcontextprotocol/clientInfo"
const capabilitiesMetaKey = "io.modelcontextprotocol/clientCapabilities"
const serverInfoMetaKey = "io.modelcontextprotocol/serverInfo"
const subscriptionMetaKey = "io.modelcontextprotocol/subscriptionId"

func clientInfo() map[string]string { return map[string]string{"name": "dsgo", "version": "0.1.0-dev"} }

type discoverResult struct {
	SupportedVersions []string                   `json:"supportedVersions"`
	Capabilities      map[string]json.RawMessage `json:"capabilities"`
	Instructions      string                     `json:"instructions,omitempty"`
	Meta              map[string]json.RawMessage `json:"_meta,omitempty"`
}

type eraVerdict struct {
	modern   bool
	discover discoverResult
}

func openNegotiated(ctx context.Context, cwd string, server state.MCPServer, options OpenOptions) (*Client, error) {
	probe, err := startTransport(cwd, server, options, true)
	if err != nil {
		return nil, err
	}
	verdict, err := probe.negotiate(ctx, options.ProbeTimeout, server.Command != "")
	if server.Command != "" {
		if closeErr := probe.Close(); closeErr != nil {
			return nil, errors.Join(err, ErrIncompleteClose, closeErr)
		}
		if err != nil {
			return nil, err
		}
		probe, err = startTransport(cwd, server, options, false)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, closeFailedClient(probe, err)
	}
	c := probe
	c.mu.Lock()
	c.probing = false
	c.probeID = ""
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, closeFailedClient(c, err)
	}
	if verdict.modern {
		c.setProtocol(ProtocolVersion)
		c.mu.Lock()
		c.Capabilities, c.Instructions = verdict.discover.Capabilities, verdict.discover.Instructions
		c.mu.Unlock()
		var info map[string]any
		if raw := verdict.discover.Meta[serverInfoMetaKey]; len(raw) > 0 && json.Unmarshal(raw, &info) == nil {
			if _, ok := info["name"].(string); ok {
				if _, ok := info["version"].(string); ok {
					c.ServerInfo = info
				}
			}
		}
	} else if err := c.legacyHandshake(ctx); err != nil {
		return nil, closeFailedClient(c, err)
	}
	if !options.skipInstructionLimit && len([]byte(c.Instructions)) > options.MaxInstructionBytes {
		return nil, closeFailedClient(c, fmt.Errorf("MCP server instructions exceed %d bytes", options.MaxInstructionBytes))
	}
	if verdict.modern {
		// The listen ack is connect-scoped; its stream has the Client lifetime.
		// A refused/unsupported listen is soft, while caller cancellation is fatal.
		if err := c.openToolsSubscription(ctx); err != nil && ctx.Err() != nil {
			return nil, closeFailedClient(c, ctx.Err())
		}
	} else if c.http != nil {
		c.httpWG.Add(1)
		go func() { defer c.httpWG.Done(); c.readHTTPEvents() }()
	}
	return c, nil
}

func (c *Client) setProtocol(version string) {
	c.mu.Lock()
	c.ProtocolVersion = version
	c.mu.Unlock()
	if c.http != nil {
		c.http.mu.Lock()
		c.http.protocol = version
		c.http.mu.Unlock()
	}
}

func (c *Client) modern() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ProtocolVersion == ProtocolVersion
}

func (c *Client) legacyHandshake(ctx context.Context) error {
	var result struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      map[string]any             `json:"serverInfo"`
		Instructions    string                     `json:"instructions"`
	}
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": LegacyProtocolVersion, "capabilities": map[string]any{}, "clientInfo": clientInfo()}, &result); err != nil {
		return err
	}
	if !supportedProtocol(result.ProtocolVersion) || result.ProtocolVersion == ProtocolVersion {
		return fmt.Errorf("unsupported legacy MCP protocol version %q", result.ProtocolVersion)
	}
	if result.Capabilities == nil {
		return errors.New("MCP initialize omitted capabilities")
	}
	if _, ok := result.ServerInfo["name"].(string); !ok {
		return errors.New("MCP initialize omitted serverInfo.name")
	}
	if _, ok := result.ServerInfo["version"].(string); !ok {
		return errors.New("MCP initialize omitted serverInfo.version")
	}
	c.setProtocol(result.ProtocolVersion)
	c.mu.Lock()
	c.Capabilities, c.ServerInfo, c.Instructions = result.Capabilities, result.ServerInfo, result.Instructions
	c.mu.Unlock()
	return c.notify(ctx, "notifications/initialized", nil)
}

func (c *Client) negotiate(ctx context.Context, timeout time.Duration, stdio bool) (eraVerdict, error) {
	if timeout == 0 {
		timeout = requestTimeout
	}
	if timeout < 0 {
		return eraVerdict{}, errors.New("invalid MCP probe timeout")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		id := "server-discover-probe-" + strconv.Itoa(attempt)
		c.mu.Lock()
		c.probeID = id
		c.mu.Unlock()
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		var raw json.RawMessage
		err := c.callWithID(probeCtx, "server/discover", nil, &raw, json.RawMessage(strconv.Quote(id)), true)
		cancel()
		if ctx.Err() != nil {
			return eraVerdict{}, ctx.Err()
		}
		if err == nil {
			var value discoverResult
			if json.Unmarshal(raw, &value) != nil || value.SupportedVersions == nil || value.Capabilities == nil {
				return eraVerdict{}, nil
			}
			if validateResultWire("server/discover", raw, true) != nil {
				return eraVerdict{}, nil
			}
			var object map[string]json.RawMessage
			_ = json.Unmarshal(raw, &object)
			if kind, ok := object["resultType"]; ok {
				var text string
				if json.Unmarshal(kind, &text) != nil || string(kind) == "null" {
					return eraVerdict{}, nil
				}
			}
			for _, version := range value.SupportedVersions {
				if version == ProtocolVersion {
					return eraVerdict{modern: true, discover: value}, nil
				}
			}
			return eraVerdict{}, nil
		}
		var status *HTTPStatusError
		if errors.As(err, &status) {
			if status.StatusCode == 401 || status.StatusCode == 403 || status.StatusCode >= 500 {
				return eraVerdict{}, err
			}
			var response wire
			if json.Unmarshal(status.Body, &response) == nil && response.Error != nil {
				err = response.Error
			} else {
				return eraVerdict{}, nil
			}
		}
		var rpc *RPCError
		if errors.As(err, &rpc) {
			if rpc.Code != -32022 {
				return eraVerdict{}, nil
			}
			var data struct {
				Supported []string `json:"supported"`
			}
			if json.Unmarshal(rpc.Data, &data) != nil || data.Supported == nil {
				return eraVerdict{}, nil
			}
			mutual, modernEvidence := false, false
			for _, version := range data.Supported {
				if version == ProtocolVersion {
					mutual = true
				}
				if len(version) == 10 && version >= ProtocolVersion {
					modernEvidence = true
				}
			}
			if mutual {
				if attempt == 1 {
					continue
				}
				return eraVerdict{}, fmt.Errorf("MCP protocol corrective continuation rejected: %w", err)
			}
			if modernEvidence {
				return eraVerdict{}, fmt.Errorf("MCP server offers no supported modern version: %w", err)
			}
			return eraVerdict{}, nil
		}
		if stdio {
			select {
			case <-c.done:
				return eraVerdict{}, nil
			default:
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClosed) || errors.Is(err, io.EOF) {
				return eraVerdict{}, nil
			}
		}
		return eraVerdict{}, err
	}
	return eraVerdict{}, errors.New("MCP protocol negotiation did not settle")
}

func (c *Client) params(value any, probe bool) (json.RawMessage, error) {
	input, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if !probe && !c.modern() {
		if value == nil {
			return nil, nil
		}
		return input, nil
	}
	object := map[string]json.RawMessage{}
	if value != nil {
		if err := json.Unmarshal(input, &object); err != nil || object == nil {
			return nil, errors.New("MCP params must be an object")
		}
	}
	meta := map[string]json.RawMessage{protocolMetaKey: json.RawMessage(strconv.Quote(ProtocolVersion)), clientInfoMetaKey: mustJSON(clientInfo()), capabilitiesMetaKey: json.RawMessage(`{}`)}
	if raw, ok := object["_meta"]; ok {
		var supplied map[string]json.RawMessage
		if json.Unmarshal(raw, &supplied) != nil || supplied == nil {
			return nil, errors.New("MCP _meta must be an object")
		}
		for key, value := range supplied {
			meta[key] = value
		}
	}
	object["_meta"] = mustJSON(meta)
	return json.Marshal(object)
}

func mustJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

type InputRequiredError struct {
	Method          string
	InputRequests   map[string]json.RawMessage
	RequestState    string
	HasRequestState bool
	RequestStateRaw json.RawMessage
}

func (e *InputRequiredError) Error() string {
	return "MCP " + e.Method + " requires an input_required continuation not yet supported by this adapter"
}

func (c *Client) decodeResult(method string, data json.RawMessage, out any, raw bool) error {
	if !raw && c.modern() {
		var result map[string]json.RawMessage
		if json.Unmarshal(data, &result) != nil || result == nil {
			return errors.New("invalid modern MCP result: expected object")
		}
		var kind string
		if json.Unmarshal(result["resultType"], &kind) != nil || kind == "" {
			return fmt.Errorf("invalid modern MCP %s result: missing required resultType", method)
		}
		if kind == "input_required" {
			var payload InputRequiredError
			payload.Method = method
			_ = json.Unmarshal(result["inputRequests"], &payload.InputRequests)
			_ = json.Unmarshal(result["requestState"], &payload.RequestState)
			var requestState string
			statePresent := json.Unmarshal(result["requestState"], &requestState) == nil && string(result["requestState"]) != "null"
			payload.HasRequestState = statePresent
			if statePresent {
				payload.RequestStateRaw = append(json.RawMessage(nil), result["requestState"]...)
			}
			if len(payload.InputRequests) == 0 && !statePresent {
				return errors.New("invalid input_required MCP result: missing inputRequests and requestState")
			}
			return &payload
		}
		if kind != "complete" {
			return fmt.Errorf("unsupported modern MCP resultType %q", kind)
		}
		if err := validateResultWire(method, data, true); err != nil {
			return err
		}
		switch method {
		case "tools/list", "prompts/list", "resources/list", "resources/templates/list", "resources/read":
			var ttl json.Number
			var scope string
			if json.Unmarshal(result["ttlMs"], &ttl) != nil {
				return errors.New("modern MCP cacheable result omitted ttlMs")
			}
			n, err := strconv.ParseFloat(ttl.String(), 64)
			if err != nil || n < 0 || n > 9007199254740991 || math.IsInf(n, 0) || math.Trunc(n) != n {
				return errors.New("invalid modern MCP ttlMs")
			}
			if json.Unmarshal(result["cacheScope"], &scope) != nil || (scope != "private" && scope != "public") {
				return errors.New("invalid modern MCP cacheScope")
			}
		}
		delete(result, "resultType")
		data = mustJSON(result)
	} else if !raw {
		var result map[string]json.RawMessage
		if json.Unmarshal(data, &result) == nil && result != nil {
			if _, ok := result["resultType"]; ok {
				delete(result, "resultType")
				data = mustJSON(result)
			}
		}
		if method == "tools/call" {
			data = normalizeLegacyToolResult(data)
		}
		if err := validateResultWire(method, data, false); err != nil {
			return err
		}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode MCP %s result: %w", method, err)
		}
	}
	return nil
}
