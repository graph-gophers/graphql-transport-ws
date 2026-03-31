# graphql-transport-ws

WebSocket transport for GraphQL servers in Go.

The supported protocol is `graphql-transport-ws` as defined in the specification: [GraphQL over WebSocket Protocol](https://github.com/graphql/graphql-over-http/blob/main/rfcs/GraphQLOverWebSocket.md).

## Getting Started

Run the bundled example server:

```bash
go run ./example
```

Then open:

`http://localhost:8080`

From the GraphiQL page, run this subscription:

```graphql
subscription {
  ticks(count: 10) {
    at
    number
  }
}
```

## With graphql-go

Wrap your HTTP GraphQL handler with `graphqlws.NewHandlerFunc`:

```go
schema := graphql.MustParseSchema(schemaSDL, &resolver{})
h := &relay.Handler{Schema: schema}

http.Handle("/graphql", graphqlws.NewHandlerFunc(schema, h))
```

Non-WebSocket requests are passed through to the wrapped HTTP handler, so the same endpoint can serve regular GraphQL HTTP traffic and WebSocket subscriptions.

See [example/server.go](./example/server.go) or [example_test.go](./example_test.go) for a runnable server using `github.com/graph-gophers/graphql-go`.

## Client notes

Connect to the GraphQL endpoint (e.g. `/graphql`) with WebSocket subprotocol `graphql-transport-ws`.

## Production considerations

- Each WebSocket connection is handled by a single backend replica, and active subscription state is kept in memory for that connection.
- If a backend node is rotated or dies, client connections to that node are dropped and in-flight subscriptions end.
- Clients should reconnect, send `connection_init` again, and resubscribe. Resuming from the exact prior event is not built in.
- If you need continuity after reconnects, implement application-level replay (for example, cursors/offsets backed by a durable event source).
- In production, set `WithCheckOrigin(...)` and consider limits/timeouts such as `WithMaxSubscriptions`, `WithReadLimit`, `WithReadIdleTimeout`, and `WithWriteTimeout`.
