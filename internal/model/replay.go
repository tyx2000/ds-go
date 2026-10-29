package model

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"net/http"
	"sort"
	"strings"
)

// The official replay envelope validates adapter/protocol, provider and model,
// and degrades unavailable metadata to provider-neutral content. This private
// tag additionally binds Go's durable opaque values to the effective connection.
// HMAC keeps exported histories from becoming credential-guessing oracles. The
// key is never part of a request, response, config profile or session history.
func replayKey(provided []byte) ([]byte, error) {
	if len(provided) != 0 && len(provided) != 32 {
		return nil, &Error{Code: "INVALID_REPLAY_KEY"}
	}
	key := make([]byte, 32)
	if len(provided) != 0 {
		copy(key, provided)
		return key, nil
	}
	if _, err := rand.Read(key); err != nil {
		return nil, &Error{Code: "REPLAY_KEY_UNAVAILABLE"}
	}
	return key, nil
}

func replayPart(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

func routeReplayIdentity(key []byte, protocol, provider, endpoint, mode, credential string, headers http.Header) string {
	h := hmac.New(sha256.New, key)
	for _, part := range []string{"dsgo-replay-v1", protocol, provider, endpoint, mode, credential} {
		replayPart(h, part)
	}
	// Header names are case insensitive on the wire. Preserve values and their
	// order, including secret tenant/organization headers, only inside the MAC.
	canonical := make(map[string][]string, len(headers))
	for name, values := range headers {
		name = strings.ToLower(name)
		canonical[name] = append(canonical[name], values...)
	}
	names := make([]string, 0, len(canonical))
	for name := range canonical {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		replayPart(h, name)
		var count [8]byte
		binary.BigEndian.PutUint64(count[:], uint64(len(canonical[name])))
		_, _ = h.Write(count[:])
		for _, value := range canonical[name] {
			replayPart(h, value)
		}
	}
	return "hmac-sha256-v1:" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (c *Client) replayIdentity(provider string) string {
	if provider == "" {
		provider = "deepseek-official"
	}
	mode, credential := "x-api-key", c.apiKey
	if c.token != "" {
		mode, credential = "x-dsh-auth-token", c.token
	}
	// root is the exact validated request root, including the transport's /v1
	// normalization. No path segment or escaping is discarded for identity.
	return routeReplayIdentity(c.replayKey, "deepseek-messages", provider, c.root, mode, credential, nil)
}

func (p *ProviderClient) replayIdentity(provider, credential string) string {
	if provider == "" {
		provider = p.options.API
	}
	mode := "bearer"
	if p.options.API == "anthropic-messages" {
		mode = "x-api-key"
	}
	return routeReplayIdentity(p.transport.replayKey, p.options.API, provider, p.transport.root, mode, credential, p.headers)
}

func sameReplayIdentity(message, request string) bool {
	return message != "" && request != "" && hmac.Equal([]byte(message), []byte(request))
}
