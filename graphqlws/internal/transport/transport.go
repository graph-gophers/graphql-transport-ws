package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/gql"
)

// operationMap holds active subscriptions.
type operationMap struct {
	ops map[string]func()
	mtx *sync.RWMutex
}

func newOperationMap() operationMap {
	return operationMap{
		ops: make(map[string]func()),
		mtx: &sync.RWMutex{},
	}
}

func (o *operationMap) add(name string, done func()) {
	o.mtx.Lock()
	o.ops[name] = done
	o.mtx.Unlock()
}

func (o *operationMap) get(name string) (func(), bool) {
	o.mtx.RLock()
	f, ok := o.ops[name]
	o.mtx.RUnlock()
	return f, ok
}

func (o *operationMap) delete(name string) {
	o.mtx.Lock()
	delete(o.ops, name)
	o.mtx.Unlock()
}

type operationMessageType string

// https://github.com/graphql/graphql-over-http/blob/main/rfcs/GraphQLOverWebSocket.md
const (
	typeConnectionInit operationMessageType = "connection_init"
	typeConnectionAck  operationMessageType = "connection_ack"
	typePing           operationMessageType = "ping"
	typePong           operationMessageType = "pong"
	typeSubscribe      operationMessageType = "subscribe"
	typeNext           operationMessageType = "next"
	typeError          operationMessageType = "error"
	typeComplete       operationMessageType = "complete"
)

type operationMessage struct {
	ID      string               `json:"id,omitempty"`
	Payload json.RawMessage      `json:"payload,omitempty"`
	Type    operationMessageType `json:"type"`
}

type subscribeMessagePayload struct {
	OperationName string                 `json:"operationName"`
	Query         string                 `json:"query"`
	Variables     map[string]interface{} `json:"variables"`
}

type wsConnection interface {
	Close() error
	ReadJSON(v interface{}) error
	SetReadLimit(limit int64)
	SetWriteDeadline(t time.Time) error
	WriteJSON(v interface{}) error
}

type connection struct {
	cancel       func()
	service      gql.GraphQLService
	writeTimeout time.Duration
	ws           wsConnection
}

type sendFunc func(id string, omType operationMessageType, payload json.RawMessage)

type Option func(conn *connection)

func ReadLimit(limit int64) Option {
	return func(conn *connection) {
		conn.ws.SetReadLimit(limit)
	}
}

func WriteTimeout(d time.Duration) Option {
	return func(conn *connection) {
		conn.writeTimeout = d
	}
}

func Connect(ctx context.Context, ws wsConnection, service gql.GraphQLService, options ...Option) {
	conn := &connection{
		service: service,
		ws:      ws,
	}

	defaultOpts := []Option{
		ReadLimit(4096),
		WriteTimeout(time.Second * 3),
	}

	for _, opt := range append(defaultOpts, options...) {
		opt(conn)
	}

	ctx, cancel := context.WithCancel(ctx)
	conn.cancel = cancel
	conn.readLoop(ctx, conn.writeLoop(ctx))
}

func (conn *connection) writeLoop(ctx context.Context) sendFunc {
	stop := make(chan struct{})
	out := make(chan *operationMessage, 1) // Using a small buffer can sometimes help, but is not essential for the fix.

	send := func(id string, omType operationMessageType, payload json.RawMessage) {
		select {
		case <-stop:
			return
		case out <- &operationMessage{ID: id, Type: omType, Payload: payload}:
		}
	}

	go func() {
		defer close(stop)
		defer conn.ws.Close()

		for {
			select {
			case msg := <-out:
				if err := conn.ws.SetWriteDeadline(time.Now().Add(conn.writeTimeout)); err != nil {
					return
				}
				if err := conn.ws.WriteJSON(msg); err != nil {
					return
				}
			case <-ctx.Done():
				// Context is canceled. Drain any remaining messages in the out channel.
				for {
					select {
					case msg := <-out:
						// Still attempt to write pending messages
						conn.ws.SetWriteDeadline(time.Now().Add(conn.writeTimeout))
						if err := conn.ws.WriteJSON(msg); err != nil {
							// On error, we can't do much more, so exit.
							return
						}
					default:
						// The out channel is empty, we can now safely exit the goroutine.
						return
					}
				}
			}
		}
	}()

	return send
}

func (conn *connection) close() {
	conn.cancel()
}

func (conn *connection) drainChannel(out chan *operationMessage) {
	for {
		select {
		case msg := <-out:
			conn.ws.SetWriteDeadline(time.Now().Add(conn.writeTimeout))
			if err := conn.ws.WriteJSON(msg); err != nil {
				return
			}
		default:
			return
		}
	}
}

func (conn *connection) readLoop(ctx context.Context, send sendFunc) {
	defer conn.close()

	ops := newOperationMap()
	initDone := false
	msgChan := make(chan *operationMessage)
	errChan := make(chan error, 1)

	go func() {
		for {
			var msg operationMessage
			if err := conn.ws.ReadJSON(&msg); err != nil {
				errChan <- err
				return
			}
			msgChan <- &msg
		}
	}()

	initTimer := time.NewTimer(conn.writeTimeout)
	defer initTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-errChan:
			// Read error occurred (e.g., client closed connection)
			// Find and cancel all active operations
			ops.mtx.Lock()
			for id, cancel := range ops.ops {
				cancel()
				delete(ops.ops, id)
			}
			ops.mtx.Unlock()
			return
		case <-initTimer.C:
			if !initDone {
				// Client failed to send connection_init in time
				send("", typeError, errPayload(errors.New("connection initialisation timeout")))
				return
			}
		case msg := <-msgChan:
			if !initDone {
				initTimer.Stop()

				if msg.Type != typeConnectionInit {
					send("", typeError, errPayload(errors.New("connection_init message not received")))
					return
				}

				// TODO: Add payload handling for auth here if needed
				send("", typeConnectionAck, nil)
				initDone = true

				continue
			}

			err := conn.processMessages(ctx, msg, send, ops)
			if err != nil {
				return
			}
		}
	}
}

// processMessages handles the different types messages in the graphql-transport-ws subprotocol
func (conn *connection) processMessages(ctx context.Context, msg *operationMessage, send sendFunc, ops operationMap) error {
	switch msg.Type {
	case typeConnectionInit:
		send("", typeError, errPayload(errors.New("connection_init sent twice")))
		return errors.New("error connection_init sent twice, the connection will be closed")

	case typePing:
		send("", typePong, msg.Payload)

	case typeSubscribe:
		if msg.ID == "" {
			send("", typeError, errPayload(errors.New("missing ID for subscribe operation")))
			return nil
		}

		if _, exists := ops.get(msg.ID); exists {
			send(msg.ID, typeError, errPayload(errors.New("duplicate operation ID")))
			return nil
		}

		var payload subscribeMessagePayload

		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			send(msg.ID, typeError, errPayload(fmt.Errorf("invalid subscribe payload: %w", err)))
			return nil
		}

		opCtx, opCancel := context.WithCancel(ctx)
		ops.add(msg.ID, opCancel)

		go conn.runSubscription(opCtx, msg.ID, payload, send, ops)

	case typeComplete:
		if msg.ID == "" {
			send("", typeError, errPayload(errors.New("missing ID for complete operation")))
			return nil
		}

		if opCancel, ok := ops.get(msg.ID); ok {
			opCancel()
			ops.delete(msg.ID)
		}

	default:
		send(msg.ID, typeError, errPayload(fmt.Errorf("unknown message type: %s", msg.Type)))
	}

	return nil
}

func (conn *connection) runSubscription(ctx context.Context, id string, payload subscribeMessagePayload, send sendFunc, ops operationMap) {
	defer ops.delete(id)

	c, err := conn.service.Subscribe(ctx, payload.Query, payload.OperationName, payload.Variables)
	if err != nil {
		send(id, typeError, errPayload(err))
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case data, more := <-c:
			if !more {
				// Subscription stream closed
				send(id, typeComplete, nil)
				return
			}

			// Stream has data, send a 'next' message
			jsonPayload, err := json.Marshal(data)
			if err != nil {
				send(id, typeError, errPayload(fmt.Errorf("failed to marshal payload: %w", err)))
				continue
			}

			send(id, typeNext, jsonPayload)
		}
	}
}

func errPayload(err error) json.RawMessage {
	b, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{
		Message: err.Error(),
	})

	return b
}
