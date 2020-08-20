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

// ContextBuilderFunc takes a context and the http request which can be used to pull values
// out of the request context and put into the supplied context.
type ContextBuilderFunc func(ctx context.Context, r *http.Request) (context.Context, error)

// Option applies configuration when a graphql-ws subprotocol is found
type Option interface {
	apply(*options)
}

type options struct {
	contextBuilders []ContextBuilderFunc
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

func applyOptions(opts ...Option) *options {
	var o options

	for _, op := range opts {
		op.apply(&o)
	}

	return &o
}

// WithContextBuilder adds a context builder option
func WithContextBuilder(f ContextBuilderFunc) Option {
	return optionFunc(func(o *options) {
		o.contextBuilders = append(o.contextBuilders, f)
	})
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func NewHandlerFunc(svc connection.GraphQLService, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol == "graphql-ws" {
				o := applyOptions(options...)

				var err error
				ctx := context.Background()
				for _, b := range o.contextBuilders {
					ctx, err = b(ctx, r)
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
