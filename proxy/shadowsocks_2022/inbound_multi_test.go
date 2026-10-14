package shadowsocks_2022

import (
	"context"
	"testing"

	A "github.com/sagernet/sing/common/auth"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
)

func newTestInbound(users ...*protocol.MemoryUser) *MultiUserInbound {
	return &MultiUserInbound{users: users}
}

func testUser(email string) *protocol.MemoryUser {
	return &protocol.MemoryUser{Email: email, Account: &MemoryAccount{Key: email}}
}

// Without a user in the context the index used to default to 0, charging the
// traffic to the first configured user without any authentication.
func TestNewConnectionRejectsUnauthenticatedContext(t *testing.T) {
	i := newTestInbound(testUser("first"), testUser("second"))

	err := i.NewConnection(context.Background(), nil, M.Metadata{})
	assert.Error(t, err, "a context without a user must be rejected, not attributed to user 0")

	err = i.NewPacketConnection(context.Background(), nil, M.Metadata{})
	assert.Error(t, err, "a context without a user must be rejected, not attributed to user 0")
}

// An out of range index must be rejected instead of panicking.
func TestNewConnectionRejectsOutOfRangeIndex(t *testing.T) {
	i := newTestInbound(testUser("first"))

	ctx := A.ContextWithUser(context.Background(), 42)
	assert.NotPanics(t, func() {
		assert.Error(t, i.NewConnection(ctx, nil, M.Metadata{}))
		assert.Error(t, i.NewPacketConnection(ctx, nil, M.Metadata{}))
	})
}

// With no users at all the old code panicked on the empty slice.
func TestNewConnectionWithNoUsers(t *testing.T) {
	i := newTestInbound()

	ctx := A.ContextWithUser(context.Background(), 0)
	assert.NotPanics(t, func() {
		assert.Error(t, i.NewConnection(ctx, nil, M.Metadata{}))
		assert.Error(t, i.NewPacketConnection(ctx, nil, M.Metadata{}))
	})
}

// userByIndex must read the slice under the same lock AddUser/RemoveUser use.
func TestUserByIndexIsRaceFree(t *testing.T) {
	i := newTestInbound(testUser("first"))

	ctx := A.ContextWithUser(context.Background(), 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_, err := i.authenticatedUser(ctx)
			assert.NoError(t, err)
		}
	}()

	for range 200 {
		require.NoError(t, i.AddUser(context.Background(), testUser("added@example.com")))
		require.NoError(t, i.RemoveUser(context.Background(), "added@example.com"))
	}
	<-done
}

// A removed user must not be resolved through a stale index.
func TestAuthenticatedUserAfterRemoval(t *testing.T) {
	i := newTestInbound(testUser("first"), testUser("second"))
	ctx := A.ContextWithUser(context.Background(), 1)

	user, err := i.authenticatedUser(ctx)
	require.NoError(t, err)
	assert.Equal(t, "second", user.Email)

	require.NoError(t, i.RemoveUser(context.Background(), "first"))

	// RemoveUser moves the last user into the freed slot, so index 1 is gone.
	user, err = i.authenticatedUser(ctx)
	assert.Error(t, err, "a stale index must not resolve to another user")
	assert.Nil(t, user)

	user, err = i.authenticatedUser(A.ContextWithUser(context.Background(), 0))
	require.NoError(t, err)
	assert.Equal(t, "second", user.Email)
}

// The helper must not panic on a nil inbound context value either.
func TestAuthenticatedUserIgnoresInbound(t *testing.T) {
	i := newTestInbound(testUser("first"))
	ctx := session.ContextWithInbound(A.ContextWithUser(context.Background(), 0), &session.Inbound{})

	user, err := i.authenticatedUser(ctx)
	require.NoError(t, err)
	assert.Equal(t, "first", user.Email)
}
