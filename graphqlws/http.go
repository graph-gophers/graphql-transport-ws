package graphqlws

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/transport"
)

const (
	// ProtocolGraphQLTransportWS is the modern websocket subprotocol ID for GraphQL over WebSocket.
	// see https://github.com/enisdenjo/graphql-ws
	ProtocolGraphQLTransportWS = "graphql-transport-ws"
)

// defaultUpgrader accepts connections from all origins.
// WARNING: this makes the server vulnerable to Cross-Site WebSocket Hijacking
// (CSWSH) when used with cookie-based authentication. Use WithCheckOrigin in
// production to restrict connections to trusted origins.
var defaultUpgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{ProtocolGraphQLTransportWS},
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
	checkOrigin       func(*http.Request) bool
	maxOperations     int
	hasMaxOperations  bool
}

func (o *options) transportOptions() []transport.Option {
	var opts []transport.Option

	if o.hasReadLimit {
		opts = append(opts, transport.ReadLimit(o.readLimit))
	}

	if o.hasWriteTimeout {
		opts = append(opts, transport.WriteTimeout(o.writeTimeout))
	}

	if o.hasMaxOperations {
		opts = append(opts, transport.MaxOperations(o.maxOperations))
	}

	return opts
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

// WithContextGenerator specifies that the background context of the websocket connection go routine
// should be built upon by executing provided context generators.
func WithContextGenerator(f ContextGenerator) Option {
	return optionFunc(func(o *options) {
		o.contextGenerators = append(o.contextGenerators, f)
	})
}

// WithReadLimit limits the maximum size of incoming messages.
func WithReadLimit(limit int64) Option {
	return optionFunc(func(o *options) {
		o.readLimit = limit
		o.hasReadLimit = true
	})
}

// WithWriteTimeout sets a timeout for outgoing messages.
func WithWriteTimeout(d time.Duration) Option {
	return optionFunc(func(o *options) {
		o.writeTimeout = d
		o.hasWriteTimeout = true
	})
}

// WithCheckOrigin sets a custom origin-checking function for the WebSocket
// upgrader. In production, restrict connections to trusted origins to prevent
// Cross-Site WebSocket Hijacking (CSWSH), e.g.:
//
//	WithCheckOrigin(func(r *http.Request) bool {
//		return r.Header.Get("Origin") == "https://example.com"
//	})
func WithCheckOrigin(fn func(*http.Request) bool) Option {
	return optionFunc(func(o *options) {
		o.checkOrigin = fn
	})
}

// WithMaxSubscriptions limits the number of concurrent GraphQL subscriptions
// allowed per WebSocket connection. The default is 100. Pass 0 to disable
// the limit (not recommended for public-facing servers).
func WithMaxSubscriptions(n int) Option {
	return optionFunc(func(o *options) {
		o.maxOperations = n
		o.hasMaxOperations = true
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
func NewHandlerFunc(svc transport.Subscriber, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	h := NewHandler()
	return h.NewHandlerFunc(svc, httpHandler, options...)
}

// NewHandlerFunc returns an http.HandlerFunc that supports GraphQL over websockets
func (h *handler) NewHandlerFunc(svc transport.Subscriber, httpHandler http.Handler, options ...Option) http.HandlerFunc {
	o := applyOptions(options...)

	upgrader := h.Upgrader
	if o.checkOrigin != nil {
		upgrader.CheckOrigin = o.checkOrigin
	}

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
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// UPGRADE FAILED: The Upgrader has already written an error response.
			// Do not call the httpHandler
			return
		}

		switch ws.Subprotocol() {
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
