package graphqlws

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/connection"
)

const protocolGraphQLWS = "graphql-ws"

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{protocolGraphQLWS},
}

// The ContextGenerator takes a context and the http request it can be used
// to take values out of the request context and assign them to a new context
// that will be supplied to the web socket connection go routine and be accessible
// in the resolver.
// The http request context should not be modified as any changes made will
// not be accessible in the resolver.
type ContextGeneratorFunc func(context.Context, *http.Request) (context.Context, error)

// BuildContext calls f(ctx, r) and returns a context and error
func (f ContextGeneratorFunc) BuildContext(ctx context.Context, r *http.Request) (context.Context, error) {
	return f(ctx, r)
}

// A ContextGenerator handles any changes made to the the connection context prior
// to creating the web socket connection routine.
type ContextGenerator interface {
	BuildContext(context.Context, *http.Request) (context.Context, error)
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func NewHandlerFunc(svc connection.GraphQLService, httpHandler http.Handler, contextGenerator ...ContextGenerator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				ctx := context.Background()

				for _, g := range contextGenerator {
					var err error
					ctx, err = g.BuildContext(ctx, r)
					if err != nil {
						return
					}
				}

				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}

				if ws.Subprotocol() != protocolGraphQLWS {
					ws.Close()
					return
				}

				go connection.Connect(ctx, ws, svc)
				return
			}
		}

		// Fallback to HTTP
		httpHandler.ServeHTTP(w, r)
	}
}
