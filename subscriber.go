package graphqlws

import "context"

// Subscriber defines the GraphQL subscription capability required
// by the WebSocket handler. Implementations should execute a subscription
// and return a channel of results.
type Subscriber interface {
	Subscribe(ctx context.Context, doc string, operation string, vars map[string]any) (<-chan any, error)
}
