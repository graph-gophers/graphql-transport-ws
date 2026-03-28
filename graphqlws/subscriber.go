package graphqlws

import "github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/transport"

// Subscriber defines the GraphQL subscription capability required
// by the WebSocket handler. Implementations should execute a subscription
// and return a channel of results.
type Subscriber = transport.Subscriber
