package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/event"
)

type operationMessageType string

// https://github.com/apollographql/subscriptions-transport-ws/blob/a56491c6feacd96cab47b7a3df8c2cb1b6a96e36/src/message-types.ts
const (
	typeComplete            operationMessageType = "complete"
	typeConnectionAck       operationMessageType = "connection_ack"
	typeConnectionError     operationMessageType = "connection_error"
	typeConnectionInit      operationMessageType = "connection_init"
	typeConnectionKeepAlive operationMessageType = "ka"
	typeConnectionTerminate operationMessageType = "connection_terminate"
	typeData                operationMessageType = "data"
	typeError               operationMessageType = "error"
	typeStart               operationMessageType = "start"
	typeStop                operationMessageType = "stop"
)

type wsConnection interface {
	Close() error
	ReadJSON(v interface{}) error
	SetReadLimit(limit int64)
	SetWriteDeadline(t time.Time) error
	WriteJSON(v interface{}) error
}

type sendFunc func(id string, omType operationMessageType, payload json.RawMessage)

// TODO?: omitempty?
type operationMessage struct {
	ID      string               `json:"id,omitempty"`
	Payload json.RawMessage      `json:"payload,omitempty"`
	Type    operationMessageType `json:"type"`
}

type initMessagePayload struct{}

type connection struct {
	cancel       func()
	handler      event.Handler
	writeTimeout time.Duration
	ws           wsConnection
}

// ReadLimit limits the maximum size of incoming messages
func ReadLimit(limit int64) func(conn *connection) {
	return func(conn *connection) {
		conn.ws.SetReadLimit(limit)
	}
}

// WriteTimeout sets a timeout for outgoing messages
func WriteTimeout(d time.Duration) func(conn *connection) {
	return func(conn *connection) {
		conn.writeTimeout = d
	}
}

// Connect implements the apollographql subscriptions-transport-ws protocol@v0.9.4
// https://github.com/apollographql/subscriptions-transport-ws/blob/v0.9.4/PROTOCOL.md
func Connect(ws wsConnection, handler event.Handler, options ...func(conn *connection)) func() {
	conn := &connection{
		handler: handler,
		ws:      ws,
	}

	defaultOpts := []func(conn *connection){
		ReadLimit(4096),
		WriteTimeout(time.Second),
	}

	for _, opt := range append(defaultOpts, options...) {
		opt(conn)
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn.cancel = cancel
	conn.readLoop(ctx, conn.writeLoop(ctx))

	return cancel
}

func (conn *connection) writeLoop(ctx context.Context) sendFunc {
	stop := make(chan struct{})
	out := make(chan *operationMessage)

	send := func(id string, omType operationMessageType, payload json.RawMessage) {
		select {
		case <-stop:
			return
		case out <- &operationMessage{ID: id, Type: omType, Payload: payload}:
		}
	}

	go func() {
		defer close(stop)
		defer conn.close()

		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-out:
				select {
				case <-ctx.Done():
					return
				default:
				}

				if err := conn.ws.SetWriteDeadline(time.Now().Add(conn.writeTimeout)); err != nil {
					return
				}

				if err := conn.ws.WriteJSON(msg); err != nil {
					return
				}
			}
		}
	}()

	return send
}

// TODO?: export this instead of returning a simple func from Connect()
func (conn *connection) close() {
	conn.cancel()
	conn.ws.Close()
}

func (conn *connection) readLoop(ctx context.Context, send sendFunc) {
	defer conn.close()

	opDone := map[string]func(){}
	for {
		var msg operationMessage
		err := conn.ws.ReadJSON(&msg)
		if err != nil {
			return
		}

		switch msg.Type {
		case typeConnectionInit:
			var initMsg initMessagePayload
			if err := json.Unmarshal(msg.Payload, &initMsg); err != nil {
				ep := errPayload(fmt.Errorf("invalid payload for type: %s", msg.Type))
				send("", typeConnectionError, ep)
				continue
			}
			send("", typeConnectionAck, nil)

		case typeStart:
			// TODO: check an operation with the same ID hasn't been started already
			if msg.ID == "" {
				ep := errPayload(errors.New("missing ID for start operation"))
				send("", typeConnectionError, ep)
				continue
			}

			args := &event.OnOperationArgs{ID: msg.ID}
			if err := json.Unmarshal(msg.Payload, &args.Payload); err != nil {
				ep := errPayload(fmt.Errorf("invalid payload for type: %s", msg.Type))
				send(msg.ID, typeConnectionError, ep)
				continue
			}

			// TODO: ensure args.Send doesn't work after typeStop or onDone()
			args.Send = func(payload json.RawMessage) {
				send(msg.ID, typeData, payload)
			}
			// TODO: timeout this call, to guard against poor clients
			payload, onDone, err := conn.handler.OnOperation(ctx, args)
			// query or mutation
			if err != nil || payload != nil {
				func() {
					defer func() {
						if onDone != nil {
							onDone()
						}
						send(msg.ID, typeComplete, nil)
					}()

					if err != nil {
						send(msg.ID, typeError, errPayload(err))
						return
					}
					send(msg.ID, typeData, payload)
				}()
				continue
			}

			// subscription
			if onDone != nil {
				opDone[msg.ID] = onDone
			}

		case typeStop:
			onDone, ok := opDone[msg.ID]
			if ok {
				delete(opDone, msg.ID)
				onDone()
			}
			send(msg.ID, typeComplete, nil)

		case typeConnectionTerminate:
			return

		case typeConnectionKeepAlive:
			send("", typeConnectionKeepAlive, nil)

		default:
			ep := errPayload(fmt.Errorf("unknown operation message of type: %s", msg.Type))
			send(msg.ID, typeError, ep)
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
