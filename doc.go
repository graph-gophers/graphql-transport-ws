// Package graphqlws implements GraphQL over WebSocket
// using the "graphql-transport-ws" subprotocol.
//
// The package exposes an HTTP handler wrapper that upgrades WebSocket requests
// and multiplexes GraphQL operations over a single socket. Non-WebSocket
// requests are delegated to the wrapped HTTP handler, allowing a single
// endpoint (for example, "/graphql") to serve both HTTP and WebSocket traffic.
//
// Protocol behavior follows the GraphQL over WebSocket RFC, including
// connection initialisation, ping/pong, subscribe/next/error/complete flow,
// and protocol close codes for invalid client behavior.
//
// A Subscriber implementation is required to execute operations:
//
//	type Subscriber interface {
//		Subscribe(ctx context.Context, document string, operation string, variables map[string]any) (<-chan any, error)
//	}
//
// Common configuration is provided through options such as read limit, write
// timeout, custom origin checks, and maximum concurrent subscriptions per
// connection.
//
// Security note: the default upgrader accepts all origins for ease of local
// development. Production deployments should set WithCheckOrigin to restrict
// trusted origins.
//
// See README.md and example_test.go for end-to-end usage.
package graphqlws
