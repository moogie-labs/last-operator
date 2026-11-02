package gatewayv1

// PurposeSuggest is CallLLMRequest.Purpose for a rule suggestion. The gateway
// audits such a call but keeps it out of the Intent's audit index.
const PurposeSuggest = "suggest"

// PurposeTurn is CallLLMRequest.Purpose for a planning turn; "" means the same.
const PurposeTurn = "turn"
