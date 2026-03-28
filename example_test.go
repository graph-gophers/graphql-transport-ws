package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"

	"github.com/gorilla/websocket"
	graphql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws"
)

const schemaString = `
	schema {
		query: Query
		subscription: Subscription
	}

	type Query {
		hello: String!
	}

	type Subscription {
		ticks: TickEvent!
	}

	type TickEvent {
		at: String!
		count: Int!
	}
`

type rootResolver struct{}

type queryResolver struct{}

type subscriptionResolver struct{}

type tickEventResolver struct {
	at    string
	count int32
}

func (*rootResolver) Query() *queryResolver {
	return &queryResolver{}
}

func (*rootResolver) Subscription() *subscriptionResolver {
	return &subscriptionResolver{}
}

func (*queryResolver) Hello() string {
	return "hello from graphql-transport-ws"
}

func (*subscriptionResolver) Ticks(ctx context.Context) (chan *tickEventResolver, error) {
	updates := make(chan *tickEventResolver, 1)
	updates <- &tickEventResolver{at: "2026-03-28T00:00:00Z", count: 1}
	close(updates)
	return updates, nil
}

func (r *tickEventResolver) At() string {
	return r.at
}

func (r *tickEventResolver) Count() int32 {
	return r.count
}

func Example() {
	schema := graphql.MustParseSchema(schemaString, &rootResolver{})
	rh := &relay.Handler{Schema: schema}
	h := graphqlws.NewHandlerFunc(schema, rh)
	srv := httptest.NewServer(h)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialer := websocket.Dialer{Subprotocols: []string{graphqlws.ProtocolGraphQLTransportWS}}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	type message struct {
		ID      string          `json:"id,omitempty"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}

	if err := conn.WriteJSON(message{Type: "connection_init"}); err != nil {
		panic(err)
	}

	if err := conn.WriteJSON(message{
		ID:   "1",
		Type: "subscribe",
		Payload: json.RawMessage(`{
			"query": "subscription { ticks { at count } }"
		}`),
	}); err != nil {
		panic(err)
	}

	var ack message
	if err := conn.ReadJSON(&ack); err != nil {
		panic(err)
	}

	var next message
	if err := conn.ReadJSON(&next); err != nil {
		panic(err)
	}

	var complete message
	if err := conn.ReadJSON(&complete); err != nil {
		panic(err)
	}

	fmt.Println(conn.Subprotocol())
	fmt.Println(ack.Type)
	fmt.Println(string(next.Payload))
	fmt.Println(complete.Type)

	// Output:
	// graphql-transport-ws
	// connection_ack
	// {"data":{"ticks":{"at":"2026-03-28T00:00:00Z","count":1}}}
	// complete
}
