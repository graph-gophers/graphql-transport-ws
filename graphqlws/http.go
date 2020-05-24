package graphqlws

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/connection"
)

// ProtocolGraphQLWS is websocket subprotocol ID for GraphQL over WebSocket
// see https://github.com/apollographql/subscriptions-transport-ws
const ProtocolGraphQLWS = "graphql-ws"

var defaultUpgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{ProtocolGraphQLWS},
}

type handler struct {
	Upgrader websocket.Upgrader
}

// NewHandler creates new GraphQL over websocket Handler with default websocket Upgrader.
func NewHandler() handler {
	return handler{Upgrader: defaultUpgrader}
}

// The ContextGeneratorFunc takes a context and the http request it can be used
// to take values out of the request context and assign them to a new context
// that will be supplied to the websocket connection go routine and be accessible
// in the resolver.
// The http request context should not be modified as any changes made will
// not be accessible in the resolver.
type ContextGeneratorFunc func(context.Context, *http.Request) (context.Context, error)

// BuildContext calls f(ctx, r) and returns a context and error
func (f ContextGeneratorFunc) BuildContext(ctx context.Context, r *http.Request) (context.Context, error) {
	return f(ctx, r)
}

// A ContextGenerator handles any changes made to the the connection context prior
// to creating the websocket connection routine.
type ContextGenerator interface {
	BuildContext(context.Context, *http.Request) (context.Context, error)
}

// Option applies configuration when a graphql websocket connection is handled
type Option interface {
	apply(*options)
}

type options struct {
	contextGenerators []ContextGenerator
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

// WithContextGenerator specifies that the background context of the websocket connection go routine
// should be built upon by executing provided context generators
func WithContextGenerator(f ContextGenerator) Option {
	return optionFunc(func(o *options) {
		o.contextGenerators = append(o.contextGenerators, f)
	})
}

func applyOptions(opts ...Option) *options {
	var o options

	for _, op := range opts {
		op.apply(&o)
	}

	return &o
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func NewHandlerFunc(svc connection.GraphQLService, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	handler := NewHandler()
	return handler.NewHandlerFunc(svc, httpHandler, options...)
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func (h *handler) NewHandlerFunc(svc connection.GraphQLService, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	o := applyOptions(options...)

	return func(w http.ResponseWriter, r *http.Request) {
		for _, subprotocol := range websocket.Subprotocols(r) {
			if subprotocol != ProtocolGraphQLWS {
				continue
			}

			ctx, err := buildContext(r, o.contextGenerators)
			ws, err := h.Upgrader.Upgrade(w, r, nil)
			if err != nil {
				w.Header().Set("X-WebSocket-Upgrade-Failure", err.Error())
				return
			}

			if ws.Subprotocol() != ProtocolGraphQLWS {
				w.Header().Set("X-WebSocket-Upgrade-Failure",
					fmt.Sprintf("upgraded websocket has wrong subprotocol (%s)", ws.Subprotocol()))
				ws.Close()
				return
			}

			go connection.Connect(ctx, ws, svc)
			return
		}

		// Fallback to HTTP
		w.Header().Set("X-WebSocket-Upgrade-Failure", "no subprotocols available")
		httpHandler.ServeHTTP(w, r)
	}
}

func buildContext(r *http.Request, generators []ContextGenerator) (context.Context, error) {
	ctx := context.Background()
	for _, g := range generators {
		var err error
		ctx, err = g.BuildContext(ctx, r)
		if err != nil {
			return nil, err
		}
	}

	return ctx, nil
}
