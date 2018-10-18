package graphqlws

import (
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/event"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/connection"
)

const protocolGraphQLWS = "graphql-ws"

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{protocolGraphQLWS},
}

// Handler is a GraphQL websocket subscription handler
type Handler struct {
	eventsHandler event.Handler
}

// NewHandler returns a new Handler
func NewHandler(eh event.Handler) *Handler {
	return &Handler{eventsHandler: eh}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	if ws.Subprotocol() != protocolGraphQLWS {
		ws.Close()
		return
	}

	go connection.Connect(ws, h.eventsHandler)

	return
}
