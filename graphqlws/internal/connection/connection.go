package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
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

type startMessagePayload struct {
	OperationName string                 `json:"operationName"`
	Query         string                 `json:"query"`
	Variables     map[string]interface{} `json:"variables"`
}

type initMessagePayload struct{}

// GraphQLService interface
type GraphQLService interface {
	Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]interface{}) (payloads <-chan interface{}, err error)
}

type connection struct {
	cancel       func()
	service      GraphQLService
	writeTimeout time.Duration
	ws           wsConnection
}

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

type Option func(conn *connection)

// ReadLimit limits the maximum size of incoming messages
func ReadLimit(limit int64) Option {
	return func(conn *connection) {
		conn.ws.SetReadLimit(limit)
	}
}

// WriteTimeout sets a timeout for outgoing messages
func WriteTimeout(d time.Duration) Option {
	return func(conn *connection) {
		conn.writeTimeout = d
	}
}

// Connect implements the apollographql subscriptions-transport-ws protocol@v0.9.4
// https://github.com/apollographql/subscriptions-transport-ws/blob/v0.9.4/PROTOCOL.md
func Connect(ctx context.Context, ws wsConnection, service GraphQLService, options ...Option) func() {
	conn := &connection{
		service: service,
		ws:      ws,
	}

	defaultOpts := []Option{
		ReadLimit(4096),
		WriteTimeout(time.Second),
	}

	for _, opt := range append(defaultOpts, options...) {
		opt(conn)
	}

	ctx, cancel := context.WithCancel(ctx)
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

func (conn *connection) addSubscription(ctx context.Context,
	cancel context.CancelFunc,
	ops operationMap,
	message operationMessage,
	send sendFunc) {
	defer cancel()
	var c <-chan interface{}
	var err error
	var mp startMessagePayload
	if err := json.Unmarshal(message.Payload, &mp); err != nil {
		ep := errPayload(fmt.Errorf("invalid payload for type: %s", message.Type))
		send(message.ID, typeConnectionError, ep)
		return
	}

	var timeout = time.NewTimer(conn.writeTimeout)
	var setupComplete = make(chan bool)
	var bail = make(chan bool)

	go func(t <-chan time.Time, kill chan bool) {
		select {
		case <-t:
			// setup timed out
			ops.delete(message.ID)
			ep := errPayload(fmt.Errorf("server subscription connect timeout after %s", conn.writeTimeout))
			send(message.ID, typeError, ep)
			send(message.ID, typeComplete, nil)
			kill <- true
		case <-setupComplete:
			// setup completed, shut down goroutine
			return
		}
	}(timeout.C, bail)

	c, err = conn.service.Subscribe(ctx, mp.Query, mp.OperationName, mp.Variables)
	if err != nil {
		ops.delete(message.ID)
		send(message.ID, typeError, errPayload(err))
		send(message.ID, typeComplete, nil)
		setupComplete <- true
		return
	}

	timeout.Stop()
	setupComplete <- true

	for {
		select {
		case <-bail:
			return
		case <-ctx.Done():
			return
		case payload, more := <-c:
			if !more {
				send(message.ID, typeComplete, nil)
				return
			}

			jsonPayload, err := json.Marshal(payload)
			if err != nil {
				send(message.ID, typeError, errPayload(err))
				continue
			}
			send(message.ID, typeData, jsonPayload)
		}
	}
}

func (conn *connection) readLoop(ctx context.Context, send sendFunc) {
	defer conn.close()

	opDone := newOperationMap()
	var header json.RawMessage

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
			header = msg.Payload

		case typeStart:
			if msg.ID == "" {
				ep := errPayload(errors.New("missing ID for start operation"))
				send("", typeConnectionError, ep)
				continue
			}

			if _, exists := opDone.get(msg.ID); exists {
				ep := errPayload(errors.New("duplicate message ID for start operation"))
				send("", typeConnectionError, ep)
				continue
			}

			opCtx, opCancel := context.WithCancel(ctx)
			opCtx = context.WithValue(opCtx, "Header", header)
			opDone.add(msg.ID, opCancel)

			go conn.addSubscription(opCtx, opCancel, opDone, msg, send)

		case typeStop:
			onDone, ok := opDone.get(msg.ID)
			if ok {
				opDone.delete(msg.ID)
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
