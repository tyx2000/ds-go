package workflow

import (
	"dsgo/internal/core"
	"encoding/json"
)

func ToolSpec() core.ToolSpec {
	return core.ToolSpec{Name: "workflow", Description: `Run a plain JavaScript workflow body with top-level await and return a JSON value. Use only when explicitly requested. Hooks: agent(prompt,{label?,phase?,provider?,model?,schema?}) runs a child to completion, returning final text or validated structured object, null for child failure. Schema supports only type/properties/required/additionalProperties/items/enum/const/oneOf with object root. parallel(thunks) awaits all functions; ordinary item exceptions become null. pipeline(items,...stages) independently processes each item through stages(prev,item,index), without a barrier; ordinary stage errors drop that item to null. Hook misuse and caps fail the run. phase(title), log(message), args are available. Scripts have no filesystem, network, timer or Node APIs.`, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"script":{"type":"string"},"meta":{"type":"object","additionalProperties":true,"properties":{"name":{"type":"string"},"description":{"type":"string"},"whenToUse":{"type":"string"},"phases":{"type":"array","items":{"type":"object","additionalProperties":true,"properties":{"title":{"type":"string"},"detail":{"type":"string"},"provider":{"type":"string"},"model":{"type":"string"}},"required":["title"]}}},"required":["name","description"]},"args":{"type":"object","additionalProperties":true},"run_in_background":{"type":"boolean"}},"required":["script","meta"]}`)}
}
