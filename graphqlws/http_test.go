package graphqlws

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextGenerators(t *testing.T) {
	contextBuilderFunc := func() ContextGeneratorFunc {
		return func(ctx context.Context, r *http.Request) (context.Context, error) {
			return context.WithValue(ctx, "testKey", "test value"), nil
		}
	}

	contextBuilderErrorFunc := func() ContextGeneratorFunc {
		return func(ctx context.Context, r *http.Request) (context.Context, error) {
			return nil, errors.New("unexpected error generating context")
		}
	}

	type args struct {
		Generator []ContextGenerator
	}
	type want struct {
		Context context.Context
		Error string
	}

	testTable := map[string]struct {
		Args args
		Want want
	}{
		"No_options": {
			Want: want{Context: context.Background()},
		},
		"With_context_generators": {
			Args: args{Generator: []ContextGenerator{contextBuilderFunc()}},
			Want: want{Context: context.WithValue(context.Background(), "testKey", "test value")},
		},
		"With_context_generator_error": {
			Args: args{Generator: []ContextGenerator{contextBuilderErrorFunc()}},
			Want: want{Context: nil, Error: "unexpected error generating context"},
		},
	}

	for name, tt := range testTable {
		tt := tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest("GET", "/graphql", nil)
			require.NoError(t, err, "Failed to create request")

			ctx, err := buildContext(req, tt.Args.Generator)

			if tt.Want.Error != "" {
				assert.EqualError(t, err, tt.Want.Error, "Expected error")
				return
			}
			assert.Equal(t, tt.Want.Context, ctx, "New context generated")
			require.NoError(t, err, "Error generating context")
		})

	}
}