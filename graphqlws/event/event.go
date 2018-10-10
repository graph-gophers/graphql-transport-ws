package event

import (
	"context"
	"encoding/json"
)

// Handler handles graphqlws events
type Handler interface {
	OnOperation(ctx context.Context, args *OnOperationArgs) (payload json.RawMessage, onDone func(), err error)
}

// OnOperationArgs are the inputs available to the OnOperation event handler
type OnOperationArgs struct {
	OperationID  string
	Send         func(payload json.RawMessage)
	StartMessage struct {
		OperationName string                     `json:"operationName"`
		Query         string                     `json:"query"`
		Variables     map[string]json.RawMessage `json:"variables"`
	}
}
