package models

import (
	"context"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCrossSporkClient_MultiClient(t *testing.T) {
	clientHosts := []string{"test1.com", "test2.com", "test3.com"}

	client, err := NewCrossSporkClient(clientHosts[0], zerolog.Nop())
	require.NoError(t, err)

	err = client.AddSpork(100, clientHosts[1])
	require.NoError(t, err)

	err = client.AddSpork(200, clientHosts[2])
	require.NoError(t, err)

	c := client.getClientForHeight(300)
	require.NotNil(t, c)

	ctx := context.Background()

	// this height should use current spork client
	_, err = client.GetBlockHeaderByHeight(ctx, 300)
	require.ErrorContains(t, err, clientHosts[0])

	// this height should use test2 client
	_, err = client.GetBlockHeaderByHeight(ctx, 150)
	require.ErrorContains(t, err, clientHosts[2])

	// this height should use test3 client
	_, err = client.GetBlockHeaderByHeight(ctx, 50)
	require.ErrorContains(t, err, clientHosts[1])

	// test boundaries are inclusive
	_, err = client.GetBlockHeaderByHeight(ctx, 200)
	require.ErrorContains(t, err, clientHosts[2])
}

func TestCrossSporkClient_ExistingHeight(t *testing.T) {
	client, err := NewCrossSporkClient("host1.com", zerolog.Nop())
	require.NoError(t, err)

	err = client.AddSpork(100, "host2.com")
	require.NoError(t, err)

	err = client.AddSpork(100, "host3.com")
	require.EqualError(t, err, "provided last height already exists")
}