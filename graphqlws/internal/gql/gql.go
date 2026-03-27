package gql

import "context"

type GraphQLService interface {
	Subscribe(ctx context.Context, document string, operationName string, variableValues map[string]any) (payloads <-chan any, err error)
}
