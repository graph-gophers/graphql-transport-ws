package graphqlws

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/transport"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/connection"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/gql"
)

const (
	// ProtocolGraphQLTransportWS is the modern websocket subprotocol ID for GraphQL over WebSocket.
	// see https://github.com/enisdenjo/graphql-ws
	ProtocolGraphQLTransportWS = "graphql-transport-ws"
	// ProtocolGraphQLWS is the deprecated websocket subprotocol ID for GraphQL over WebSocket.
	// see https://github.com/apollographql/subscriptions-transport-ws
	ProtocolGraphQLWS = "graphql-ws"
)

var defaultUpgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{ProtocolGraphQLTransportWS, ProtocolGraphQLWS},
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
	readLimit         int64
	writeTimeout      time.Duration
	hasReadLimit      bool
	hasWriteTimeout   bool
}

func (o *options) connectionOptions() []connection.Option {
	var connOptions []connection.Option

	if o.hasReadLimit {
		connOptions = append(connOptions, connection.ReadLimit(o.readLimit))
	}

	if o.hasWriteTimeout {
		connOptions = append(connOptions, connection.WriteTimeout(o.writeTimeout))
	}

	return connOptions
}

func (o *options) transportOptions() []transport.Option {
	var connOptions []transport.Option

	if o.hasReadLimit {
		connOptions = append(connOptions, transport.ReadLimit(o.readLimit))
	}

	if o.hasWriteTimeout {
		connOptions = append(connOptions, transport.WriteTimeout(o.writeTimeout))
	}

	return connOptions
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

// WithReadLimit limits the maximum size of incoming messages
func WithReadLimit(limit int64) Option {
	return optionFunc(func(o *options) {
		o.readLimit = limit
		o.hasReadLimit = true
	})
}

// WithWriteTimeout sets a timeout for outgoing messages
func WithWriteTimeout(d time.Duration) Option {
	return optionFunc(func(o *options) {
		o.writeTimeout = d
		o.hasWriteTimeout = true
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
func NewHandlerFunc(svc gql.GraphQLService, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	handler := NewHandler()
	return handler.NewHandlerFunc(svc, httpHandler, options...)
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func (h *handler) NewHandlerFunc(svc gql.GraphQLService, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	o := applyOptions(options...)

	return func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			if httpHandler == nil {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}

			httpHandler.ServeHTTP(w, r)
			return
		}

		ctx, err := buildContext(r, o.contextGenerators)
		if err != nil {
			w.Header().Set("X-WebSocket-Upgrade-Failure", err.Error())
			return
		}

		ws, err := h.Upgrader.Upgrade(w, r, nil)
		if err != nil {
			// UPGRADE FAILED: The Upgrader has already written an error response.
			// Do not call the httpHandler
			return
		}

		switch ws.Subprotocol() {
		case ProtocolGraphQLWS:
			go connection.Connect(ctx, ws, svc, o.connectionOptions()...)

		case ProtocolGraphQLTransportWS:
			go transport.Connect(ctx, ws, svc, o.transportOptions()...)

		default:
			w.Header().Set("X-WebSocket-Upgrade-Failure", "unsupported subprotocol")
			ws.Close()
		}
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
