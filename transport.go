package graphqlws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// operationMap holds active subscriptions.
type operationMap struct {
	ops map[string]func()
	mu  *sync.RWMutex
}

func newOperationMap() operationMap {
	return operationMap{
		ops: make(map[string]func()),
		mu:  &sync.RWMutex{},
	}
}

func (o *operationMap) add(name string, done func()) {
	o.mu.Lock()
	o.ops[name] = done
	o.mu.Unlock()
}

func (o *operationMap) get(name string) (func(), bool) {
	o.mu.RLock()
	f, ok := o.ops[name]
	o.mu.RUnlock()
	return f, ok
}

func (o *operationMap) delete(name string) {
	o.mu.Lock()
	delete(o.ops, name)
	o.mu.Unlock()
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

const (
	closeCodeBadRequest                = 4400
	closeCodeUnauthorized              = 4401
	closeCodeConnectionInitTimeout     = 4408
	closeCodeSubscriberAlreadyExists   = 4409
	closeCodeTooManyInitialisationReqs = 4429
)

type operationMessage struct {
	ID      string               `json:"id,omitempty"`
	Payload json.RawMessage      `json:"payload,omitempty"`
	Type    operationMessageType `json:"type"`
}

type subscribeMessagePayload struct {
	OperationName string         `json:"operationName"`
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
}

type wsConnection interface {
	Close() error
	ReadJSON(v any) error
	SetReadLimit(limit int64)
	SetWriteDeadline(t time.Time) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	WriteJSON(v any) error
}

type connection struct {
	cancel       func()
	maxOps       int
	sub          Subscriber
	writeTimeout time.Duration
	ws           wsConnection
}

type sendFunc func(id string, omType operationMessageType, payload json.RawMessage)

type transportOption func(conn *connection)

func transportReadLimit(limit int64) transportOption {
	return func(conn *connection) {
		conn.ws.SetReadLimit(limit)
	}
}

func transportWriteTimeout(d time.Duration) transportOption {
	return func(conn *connection) {
		conn.writeTimeout = d
	}
}

// transportMaxOperations limits the number of concurrent subscribe operations per
// connection. A value of 0 disables the limit. Negative values are treated
// as 0 (no limit).
func transportMaxOperations(n int) transportOption {
	return func(conn *connection) {
		conn.maxOps = max(n, 0)
	}
}

func connectTransport(ctx context.Context, ws wsConnection, sub Subscriber, options ...transportOption) {
	conn := &connection{
		sub: sub,
		ws:  ws,
	}

	defaultOpts := []transportOption{
		transportReadLimit(4096),
		transportWriteTimeout(time.Second * 3),
		transportMaxOperations(100),
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

func (conn *connection) closeWithCode(code int, reason string) {
	_ = conn.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(conn.writeTimeout),
	)
	conn.cancel()
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
		case err := <-errChan:
			// Read error occurred (e.g., client closed connection)
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) && !errors.Is(err, io.EOF) && err.Error() != "connection closed" {
				conn.closeWithCode(closeCodeBadRequest, "invalid message")
			}
			// Find and cancel all active operations
			ops.mu.Lock()
			for id, cancel := range ops.ops {
				cancel()
				delete(ops.ops, id)
			}
			ops.mu.Unlock()
			return
		case <-initTimer.C:
			if !initDone {
				// Client failed to send connection_init in time.
				conn.closeWithCode(closeCodeConnectionInitTimeout, "Connection initialisation timeout")
				return
			}
		case msg := <-msgChan:
			if !initDone {
				initTimer.Stop()

				if msg.Type != typeConnectionInit {
					conn.closeWithCode(closeCodeUnauthorized, "Unauthorized")
					return
				}

				if len(msg.Payload) > 0 {
					var initPayload map[string]any
					if err := json.Unmarshal(msg.Payload, &initPayload); err != nil {
						conn.closeWithCode(closeCodeBadRequest, "invalid connection_init payload")
						return
					}
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
		conn.closeWithCode(closeCodeTooManyInitialisationReqs, "Too many initialisation requests")
		return errors.New("connection_init sent twice")

	case typePing:
		send("", typePong, msg.Payload)

	case typePong:
		// Pong can be sent unsolicited by either peer; ignore.

	case typeSubscribe:
		if msg.ID == "" {
			conn.closeWithCode(closeCodeBadRequest, "missing ID for subscribe operation")
			return errors.New("missing ID for subscribe operation")
		}

		if _, exists := ops.get(msg.ID); exists {
			conn.closeWithCode(closeCodeSubscriberAlreadyExists, fmt.Sprintf("Subscriber for %s already exists", msg.ID))
			return errors.New("duplicate operation ID")
		}

		if conn.maxOps > 0 {
			ops.mu.RLock()
			count := len(ops.ops)
			ops.mu.RUnlock()
			if count >= conn.maxOps {
				send(msg.ID, typeError, errPayload(errors.New("too many concurrent subscriptions")))
				return nil
			}
		}

		var payload subscribeMessagePayload

		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			conn.closeWithCode(closeCodeBadRequest, "invalid subscribe payload")
			return errors.New("invalid subscribe payload")
		}

		opCtx, opCancel := context.WithCancel(ctx)
		ops.add(msg.ID, opCancel)

		go conn.runSubscription(opCtx, msg.ID, payload, send, ops)

	case typeComplete:
		if msg.ID == "" {
			conn.closeWithCode(closeCodeBadRequest, "missing ID for complete operation")
			return errors.New("missing ID for complete operation")
		}

		if opCancel, ok := ops.get(msg.ID); ok {
			opCancel()
			ops.delete(msg.ID)
		}

	default:
		conn.closeWithCode(closeCodeBadRequest, fmt.Sprintf("unknown message type: %s", msg.Type))
		return errors.New("unknown message type")
	}

	return nil
}

func (conn *connection) runSubscription(ctx context.Context, id string, payload subscribeMessagePayload, send sendFunc, ops operationMap) {
	defer ops.delete(id)

	c, err := conn.sub.Subscribe(ctx, payload.Query, payload.OperationName, payload.Variables)
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
	b, _ := json.Marshal([]map[string]string{{
		"message": err.Error(),
	}})

	return b
}
