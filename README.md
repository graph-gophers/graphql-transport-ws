# graphql-transport-ws
[![Build Status](https://travis-ci.org/graph-gophers/graphql-transport-ws.svg?branch=master)](https://travis-ci.org/graph-gophers/graphql-transport-ws)

**(Work in progress!)**

A Go package that leverages WebSockets to transport GraphQL subscriptions, queries and mutations implementing the [Apollo@v0.9.4 protocol](https://github.com/apollographql/subscriptions-transport-ws/blob/v0.9.4/PROTOCOL.md)

### Use with graph-gophers/graphql-go

To use this library with [github.com/graph-gophers/graphql-go](https://github.com/graph-gophers/graphql-go) you can wrap the `relay` handler it provides the following way:

```
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
	graphql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws/event"
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
	graphQLHandler := newHandler(s, &relay.Handler{Schema: s})
	http.HandleFunc("/graphql", graphQLHandler)

	// start HTTP server
	if err := http.ListenAndServe(fmt.Sprintf(":%d", 8080), nil); err != nil {
		panic(err)
	}
}

func newHandler(s *graphql.Schema, httpHandler http.Handler) http.HandlerFunc {
	wsHandler := graphqlws.NewHandler(&defaultCallback{schema: s})
	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				wsHandler.ServeHTTP(w, r)
				return
			}
		}
		httpHandler.ServeHTTP(w, r)
	}
}

type defaultCallback struct {
	schema *graphql.Schema
}

func (h *defaultCallback) OnOperation(ctx context.Context, args *event.OnOperationArgs) (json.RawMessage, func(), error) {
	b, err := json.Marshal(args.StartMessage.Variables)
	if err != nil {
		return nil, nil, err
	}

	variables := map[string]interface{}{}
	err = json.Unmarshal(b, &variables)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	c, err := h.schema.Subscribe(ctx, args.StartMessage.Query, args.StartMessage.OperationName, variables)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case response, more := <-c:
				if !more {
					return
				}
				responseJSON, err := json.Marshal(response)
				if err != nil {
					args.Send(json.RawMessage(`{"errors":["internal error: can't marshal response into json"]}`))
					continue
				}
				args.Send(responseJSON)
			}
		}
	}()

	return nil, cancel, nil
}
```

For a more in depth example see [this repo](https://github.com/matiasanaya/go-graphql-subscription-example).

### Client

Check [apollographql/subscription-transport-ws](https://github.com/apollographql/subscriptions-transport-ws) for details on how to use WebSockets on the client side.
