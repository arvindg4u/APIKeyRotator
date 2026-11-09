package services

import (
	"encoding/json"
	"strings"
)

// FreeModelFallbackBody rewrites an OpenAI/Anthropic-style request body from a
// free model variant (ending in ":free") to its paid counterpart.
//
// Token Harbor returns HTTP 429 free_tier_limit_reached with the explicit
// instruction to drop ":free" and run the request on the account's plan or
// balance. Key rotation alone cannot create free allowance, so the LLM proxy
// can use this to retry the same request on the paid route.
func FreeModelFallbackBody(body []byte) (newBody []byte, newModel string, ok bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false
	}

	rawModel, exists := payload["model"]
	if !exists {
		return nil, "", false
	}

	var model string
	if err := json.Unmarshal(rawModel, &model); err != nil {
		return nil, "", false
	}

	if !strings.HasSuffix(strings.ToLower(model), ":free") {
		return nil, "", false
	}
	newModel = model[:len(model)-len(":free")]
	if newModel == "" {
		return nil, "", false
	}

	encoded, err := json.Marshal(newModel)
	if err != nil {
		return nil, "", false
	}
	payload["model"] = encoded

	newBody, err = json.Marshal(payload)
	if err != nil {
		return nil, "", false
	}
	return newBody, newModel, true
}
