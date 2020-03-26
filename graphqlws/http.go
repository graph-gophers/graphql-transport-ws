package graphqlws

import (
	"github.com/gorilla/websocket"
	"net/http"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/connection"
)

const protocolGraphQLWS = "graphql-ws"

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{protocolGraphQLWS},
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func NewHandlerFunc(svc connection.GraphQLService, httpHandler http.Handler, authFunc connection.AuthenticateFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}

				if ws.Subprotocol() != protocolGraphQLWS {
					ws.Close()
					return
				}

				if authFunc != nil {
					go connection.Connect(ws, svc, connection.Authentication(r, authFunc))
					return
				}
				go connection.Connect(ws, svc, nil)
				return
			}
		}

		// Fallback to HTTP
		httpHandler.ServeHTTP(w, r)
	}
}
