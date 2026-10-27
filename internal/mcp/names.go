package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf16"
)

// PublicToolName follows the pinned upstream v1 identity algorithm. Keep a
// separate public-to-raw map; a normalized name must never be parsed on calls.
// Source: deepseek-harness packages/mcp/mcp-client/src/tools.ts at 687f282e.
func PublicToolName(serverName, rawName string) string {
	joined := "mcp__" + serverName + "__" + rawName
	units := utf16.Encode([]rune(joined))
	normalized := make([]byte, len(units))
	changed := false
	for i, r := range units {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			normalized[i] = byte(r)
		} else {
			normalized[i] = '_'
			changed = true
		}
	}
	if !changed && len(normalized) <= 64 {
		return string(normalized)
	}
	hash := sha256.Sum256([]byte(serverName + "\x00" + rawName))
	if len(normalized) > 51 {
		normalized = normalized[:51]
	}
	return string(normalized) + "_" + hex.EncodeToString(hash[:])[:12]
}
