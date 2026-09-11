package remote

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDialRequiresEndpointAndCredentials(t *testing.T) {
	_, err := Dial(context.Background(), DialConfig{})
	require.Error(t, err)
	_, err = Dial(context.Background(), DialConfig{Endpoint: "localhost:9000"})
	require.Error(t, err)
}
