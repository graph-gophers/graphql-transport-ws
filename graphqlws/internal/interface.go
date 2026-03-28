package internal

import "context"

// Subscriber defines the GraphQL subscription capability required
// by the WebSocket handler. Implementations should execute a subscription
// and return a channel of results.
type Subscriber interface {
	Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]any) (<-chan any, error)
}
