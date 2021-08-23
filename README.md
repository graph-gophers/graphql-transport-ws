# graphql-transport-ws
[![Build Status](https://travis-ci.org/graph-gophers/graphql-transport-ws.svg?branch=master)](https://travis-ci.org/graph-gophers/graphql-transport-ws)

**(Work in progress!)**

A Go package that leverages WebSockets to transport GraphQL subscriptions, queries and mutations implementing the [Apollo@v0.9.4 protocol](https://github.com/apollographql/subscriptions-transport-ws/blob/v0.9.4/PROTOCOL.md)

### Use with graph-gophers/graphql-go

To use this library with [github.com/graph-gophers/graphql-go](https://github.com/graph-gophers/graphql-go) you can wrap the `relay` handler it provides the following way:

```go
package main

import (
	"fmt"
	"net/http"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws"
)

const schema = `
	schema {
		subscription: Subscription
	}

	type Subscription {
		...
	}
`

type resolver struct {
	// ...
}

func main() {
	// init graphQL schema
	s, err := graphql.ParseSchema(schema, &resolver{})
	if err != nil {
		panic(err)
	}

	// graphQL handler
	graphQLHandler := graphqlws.NewHandlerFunc(s, &relay.Handler{Schema: s})
	http.HandleFunc("/graphql", graphQLHandler)

	// start HTTP server
	if err := http.ListenAndServe(fmt.Sprintf(":%d", 8080), nil); err != nil {
		panic(err)
	}
}
```

For a more in depth example see [this repo](https://github.com/matiasanaya/go-graphql-subscription-example).

### Client

Check [apollographql/subscription-transport-ws](https://github.com/apollographql/subscriptions-transport-ws) for details on how to use WebSockets on the client side.
