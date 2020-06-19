package connection_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/graph-gophers/graphql-transport-ws/graphqlws/internal/connection"
)

type messageIntention int

const (
	clientSends messageIntention = 0
	expectation messageIntention = 1
)

const (
	connectionACK = `{"type":"connection_ack"}`
)

type message struct {
	intention        messageIntention
	operationMessage string
}

func TestConnect(t *testing.T) {
	testTable := []struct {
		name     string
		svc      *gqlService
		messages []message
	}{
		{
			name: "connection_init_ok",
			messages: []message{
				{
					intention: clientSends,
					operationMessage: `{
						"type":"connection_init",
						"payload":{}
					}`,
				},
				{
					intention:        expectation,
					operationMessage: connectionACK,
				},
			},
		},
		{
			name: "connection_init_error",
			messages: []message{
				{
					intention: clientSends,
					operationMessage: `{
						"type": "connection_init",
						"payload": "invalid_payload"
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"type": "connection_error",
						"payload": {
							"message": "invalid payload for type: connection_init"
						}
					}`,
				},
			},
		},
		{
			name: "start_ok",
			svc:  newGQLService(`{"data":{},"errors":null}`),
			messages: []message{
				{
					intention: clientSends,
					operationMessage: `{
						"type": "start",
						"id": "a-id",
						"payload": {}
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"type": "data",
						"id": "a-id",
						"payload": {
							"data": {},
							"errors": null
						}
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"type":"complete",
						"id": "a-id"
					}`,
				},
			},
		},
		{
			name: "start_query_data_error",
			svc:  newGQLService(`{"data":null,"errors":[{"message":"a error"}]}`),
			messages: []message{
				{
					intention: clientSends,
					// TODO?: this payload should fail?
					operationMessage: `{
						"id": "a-id",
						"type": "start",
						"payload": {}
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"id": "a-id",
						"type": "data",
						"payload": {
							"data": null,
							"errors": [{"message":"a error"}]
						}
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"type":"complete",
						"id": "a-id"
					}`,
				},
			},
		},
		{
			name: "start_query_error",
			svc: &gqlService{
				err: errors.New("some error"),
			},
			messages: []message{
				{
					intention: clientSends,
					operationMessage: `{
						"id": "a-id",
						"type": "start",
						"payload": {}
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"id": "a-id",
						"type": "error",
						"payload": {
							"message": "some error"
						}
					}`,
				},
				{
					intention: expectation,
					operationMessage: `{
						"type":"complete",
						"id": "a-id"
					}`,
				},
			},
		},
	}
	for _, tt := range testTable {
		t.Run(tt.name, func(t *testing.T) {
			ws := newConnection()
			go connection.Connect(context.Background(), ws, tt.svc)
			ws.test(t, tt.messages)
		})
	}
}

type gqlService struct {
	payloads <-chan interface{}
	err      error
}

func newGQLService(pp ...string) *gqlService {
	c := make(chan interface{}, len(pp))
	for _, p := range pp {
		c <- json.RawMessage(p)
	}
	close(c)

	return &gqlService{payloads: c}
}

func (h *gqlService) Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]interface{}) (payloads <-chan interface{}, err error) {
	return h.payloads, h.err
}

func newConnection() *wsConnection {
	return &wsConnection{
		in:  make(chan json.RawMessage),
		out: make(chan json.RawMessage),
	}
}

type wsConnection struct {
	in  chan json.RawMessage
	out chan json.RawMessage
}

func (ws *wsConnection) test(t *testing.T, messages []message) {
	for _, msg := range messages {
		switch msg.intention {
		case clientSends:
			ws.in <- json.RawMessage(msg.operationMessage)
		case expectation:
			requireEqualJSON(t, msg.operationMessage, <-ws.out)
		}
	}
}

func (ws *wsConnection) ReadJSON(v interface{}) error {
	msg := <-ws.in
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (ws *wsConnection) WriteJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ws.out <- json.RawMessage(data)
	return nil
}

func (ws *wsConnection) SetReadLimit(limit int64) {}

func (ws *wsConnection) SetWriteDeadline(t time.Time) error {
	return nil
}

func (ws *wsConnection) Close() error {
	close(ws.in)
	close(ws.out)

	return nil
}

func requireEqualJSON(t *testing.T, expected string, got json.RawMessage) {
	var expJSON interface{}
	err := json.Unmarshal([]byte(expected), &expJSON)
	if err != nil {
		t.Fatalf("error mashalling expected json: %s", err.Error())
	}

	var gotJSON interface{}
	err = json.Unmarshal(got, &gotJSON)
	if err != nil {
		t.Fatalf("error mashalling got json: %s", err.Error())
	}

	if !reflect.DeepEqual(expJSON, gotJSON) {
		normalizedExp, err := json.Marshal(expJSON)
		if err != nil {
			panic(err)
		}
		t.Fatalf("expected [%s] but instead got [%s]", normalizedExp, got)
	}
}
