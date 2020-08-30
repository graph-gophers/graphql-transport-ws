package graphqlws

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHandlerFunctionalOptions(t *testing.T) {
	contextBuilderFunc := func() ContextGeneratorFunc {
		return func(ctx context.Context, r *http.Request) (context.Context, error) {
			return context.WithValue(ctx, "testKey", "test value"), nil
		}
	}

	contextGeneratorOption := WithContextGenerator(contextBuilderFunc())

	type args struct {
		Options []Option
	}
	type want struct {
		Context context.Context
		Error error
	}

	testTable := map[string]struct {
		Args args
		Want want
	}{
		"No_options": {
			Args: args{},
			Want: want{Context: context.Background(), Error: nil},
		},
		"With_context_generators": {
			Args: args{Options: []Option{contextGeneratorOption}},
			Want: want{Context: context.WithValue(context.Background(), "testKey", "test value"), Error: nil},
		},
	}

	for name, tt := range testTable {
		tt := tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest("GET", "/graphql", nil)
			if err != nil {
				return
			}

			ctx, err := processOptions(req, tt.Args.Options...)

			if tt.Want.Error != nil {
				assert.EqualError(t, tt.Want.Error, err.Error(), "Expected error")
				return
			}
			assert.Equal(t, tt.Want.Context, ctx, "New context generated")
			require.NoError(t, err, "Error generating context")
		})

	}
}