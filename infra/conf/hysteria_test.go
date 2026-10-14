package conf_test

import (
	"testing"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	. "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/hysteria"
)

func TestHysteriaOutbound(t *testing.T) {
	runMultiTestCase(t, []TestCase{
		{
			Input: `{
				"version": 2,
				"address": "example.com",
				"port": 443
			}`,
			Parser: loadJSON(func() Buildable { return new(HysteriaClientConfig) }),
			Output: &hysteria.ClientConfig{
				Server: &protocol.ServerEndpoint{
					Address: &net.IPOrDomain{
						Address: &net.IPOrDomain_Domain{
							Domain: "example.com",
						},
					},
					Port: 443,
				},
			},
		},
	})
}

// A hysteria outbound used to accept a missing address (which builds to nil)
// and a zero port, unlike every other outbound.
func TestHysteriaOutboundInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "no address",
			input: `{"version": 2, "port": 443}`,
		},
		{
			name:  "no port",
			input: `{"version": 2, "address": "example.com"}`,
		},
		{
			name:  "wrong version",
			input: `{"version": 1, "address": "example.com", "port": 443}`,
		},
	}

	parser := loadJSON(func() Buildable { return new(HysteriaClientConfig) })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if config, err := parser(c.input); err == nil {
				t.Fatalf("expected error, but got %v", config)
			}
		})
	}
}
