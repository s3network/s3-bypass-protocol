package encryption

import (
	"bytes"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newClientInstance builds a client with a single X25519 NFS public key and a
// padding profile that does not sleep between fragments.
func newClientInstance(t *testing.T) *ClientInstance {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)

	c := &ClientInstance{}
	require.NoError(t, c.Init([][]byte{key.PublicKey().Bytes()}, 0, 0, "100-111-1111"))
	return c
}

// pipeConns returns a connected client/server pair backed by a real loopback
// socket, so writes are buffered and neither side can deadlock on the other.
func pipeConns(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	type result struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := listener.Accept()
		accepted <- result{conn, err}
	}()

	client, err = net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res := <-accepted
	require.NoError(t, res.err)
	t.Cleanup(func() { _ = res.conn.Close() })

	return client, res.conn
}

// validServerHelloBody returns 1088+32 bytes of key material that the client's
// decapsulation steps accept on their own, so only the AEAD tag check can
// reject a message built from it.
func validServerHelloBody(t *testing.T) []byte {
	t.Helper()
	decapKey, err := mlkem.GenerateKey768()
	require.NoError(t, err)
	_, ciphertext := decapKey.EncapsulationKey().Encapsulate()

	x25519Key, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)

	return append(ciphertext, x25519Key.PublicKey().Bytes()...)
}

// The client must verify the AEAD tag of the server hello. Without the check a
// tampered server hello is fed straight into the key schedule, so a wrong tag
// produces a bogus session instead of an error.
func TestHandshakeRejectsTamperedServerHello(t *testing.T) {
	c := newClientInstance(t)
	clientConn, serverConn := pipeConns(t)

	// Consume the client hello in the background so its writes can complete.
	go func() {
		scratch := make([]byte, 64*1024)
		for {
			if _, err := serverConn.Read(scratch); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.Handshake(clientConn)
		done <- err
	}()

	// A well formed key exchange body with a tag that cannot be authentic.
	time.Sleep(200 * time.Millisecond)
	serverHello := append(validServerHelloBody(t), make([]byte, 16)...)
	_, err := io.Copy(serverConn, bytes.NewReader(serverHello))
	require.NoError(t, err)

	select {
	case err := <-done:
		require.Error(t, err, "a server hello with an invalid AEAD tag must be rejected")
		// The rejection must come from the AEAD tag check itself, not from a
		// later stage that happens to choke on the zeroed plaintext.
		assert.Contains(t, err.Error(), "server hello")
	case <-time.After(20 * time.Second):
		t.Fatal("Handshake did not return after receiving a tampered server hello")
	}
}

// A server that closes the connection must produce an error, not a hang.
func TestHandshakeFailsOnClosedConn(t *testing.T) {
	c := newClientInstance(t)
	clientConn, serverConn := pipeConns(t)

	go func() {
		scratch := make([]byte, 64*1024)
		for {
			if _, err := serverConn.Read(scratch); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.Handshake(clientConn)
		done <- err
	}()

	time.Sleep(200 * time.Millisecond)
	require.NoError(t, serverConn.Close())

	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("Handshake blocked on a closed connection")
	}
}
