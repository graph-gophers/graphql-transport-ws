# graphql-transport-ws

WebSocket transport for GraphQL servers in Go.

The supported protocol is `graphql-transport-ws` as defined in the specification: [GraphQL over WebSocket Protocol](https://github.com/graphql/graphql-over-http/blob/main/rfcs/GraphQLOverWebSocket.md).

## With graphql-go

Wrap your HTTP GraphQL handler with `graphqlws.NewHandlerFunc`:

```go
schema := graphql.MustParseSchema(schemaSDL, &rootResolver{})
rh := &relay.Handler{Schema: schema}

http.Handle("/graphql", graphqlws.NewHandlerFunc(schema, rh))
```

Non-WebSocket requests are passed through to the wrapped HTTP handler, so the same endpoint can serve regular GraphQL HTTP traffic and WebSocket subscriptions.

See [example_test.go](./example_test.go) for a runnable server using `github.com/graph-gophers/graphql-go`.

## Client notes

Connect to `/graphql` with WebSocket subprotocol `graphql-transport-ws`.

Example subscription:

```graphql
subscription {
  ticks {
    at
    count
  }
}
```
