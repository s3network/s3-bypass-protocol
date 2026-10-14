package noise

import (
	"net"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/crypto"
)

// maxTrackedAddrs bounds the per-destination bookkeeping map. The key is a
// peer address that an off-path attacker controls (source addresses are
// trivially varied), so an unbounded map is a memory-exhaustion vector.
const maxTrackedAddrs = 65536

type noiseConn struct {
	net.PacketConn
	config *Config
	m      map[string]time.Time
	mu     sync.Mutex
}

func NewConnClient(c *Config, raw net.PacketConn) (net.PacketConn, error) {
	return &noiseConn{
		PacketConn: raw,
		config:     c,
		m:          make(map[string]time.Time),
	}, nil
}

func NewConnServer(c *Config, raw net.PacketConn) (net.PacketConn, error) {
	return NewConnClient(c, raw)
}

// dueForNoise reports whether addr is due for a noise burst, and records the
// next eligible time. Entries that have already elapsed are evicted so the map
// cannot grow without bound.
func (c *noiseConn) dueForNoise(addr string, now time.Time) bool {
	t, ok := c.m[addr]
	if ok && now.After(t) {
		delete(c.m, addr)
		ok = false
	}
	if ok {
		return false
	}

	// Evict the entry that has been due the longest. Map iteration order is
	// random, but under pressure any elapsed entry is better than none.
	if len(c.m) >= maxTrackedAddrs {
		for k, v := range c.m {
			if now.After(v) {
				delete(c.m, k)
				break
			}
		}
		if len(c.m) >= maxTrackedAddrs {
			// Nothing has elapsed: clear rather than grow without bound.
			clear(c.m)
		}
	}

	c.m[addr] = now.Add(time.Duration(crypto.RandBetween(c.config.ResetMin, c.config.ResetMax)) * time.Second)
	return true
}

func (c *noiseConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	// The noise burst sleeps and performs socket writes. Holding the lock
	// across that blocks every other destination for the whole burst, so the
	// bookkeeping is done under the lock and the I/O outside it.
	c.mu.Lock()
	burst := c.dueForNoise(addr.String(), time.Now())
	c.mu.Unlock()

	if burst {
		for _, item := range c.config.Items {
			if item.RandMax > 0 {
				buf := make([]byte, crypto.RandBetween(item.RandMin, item.RandMax))
				crypto.RandBytesBetween(buf, byte(item.RandRangeMin), byte(item.RandRangeMax))
				if _, err := c.PacketConn.WriteTo(buf, addr); err != nil {
					return 0, err
				}
			} else {
				if _, err := c.PacketConn.WriteTo(item.Packet, addr); err != nil {
					return 0, err
				}
			}
			time.Sleep(time.Duration(crypto.RandBetween(item.DelayMin, item.DelayMax)) * time.Millisecond)
		}
	}

	return c.PacketConn.WriteTo(p, addr)
}
