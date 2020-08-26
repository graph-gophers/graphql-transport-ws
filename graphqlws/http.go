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

// The HandlerFunc type is an adapter to allow the use
// of ordinary functions as websocket connection handlers.
type HandlerFunc func(context.Context, *http.Request) (context.Context, error)

// BuildContext calls f(ctx, r) and returns a context and error
func (f HandlerFunc) BuildContext(ctx context.Context, r *http.Request) (context.Context, error) {
	return f(ctx, r)
}

// A Handler manages the context prior to creating the websocket go routine
type Handler interface {
	BuildContext(context.Context, *http.Request) (context.Context, error)
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func NewHandlerFunc(svc connection.GraphQLService, httpHandler http.Handler, wsHandler ...Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				ctx := context.Background()

				for _, h := range wsHandler {
					var err error
					ctx, err = h.BuildContext(ctx, r)
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
