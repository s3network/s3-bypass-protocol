package outbound_test

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	. "github.com/xtls/xray-core/app/proxyman/outbound"
	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	core "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/transport"
	_ "github.com/xtls/xray-core/transport/internet/tcp"
)

type closableHandler struct {
	tag    string
	closed int
}

func (h *closableHandler) Tag() string { return h.tag }

func (h *closableHandler) Dispatch(ctx context.Context, link *transport.Link) {}

func (h *closableHandler) SenderSettings() *serial.TypedMessage { return nil }

func (h *closableHandler) ProxySettings() *serial.TypedMessage { return nil }

func (h *closableHandler) Start() error { return nil }

func (h *closableHandler) Close() error {
	h.closed++
	return nil
}

// A removed handler must be closed, like the inbound manager does, otherwise
// its resources (mux, proxy) stay alive forever.
func TestRemoveHandlerClosesHandler(t *testing.T) {
	m, err := New(context.Background(), &proxyman.OutboundConfig{})
	common.Must(err)

	handler := &closableHandler{tag: "removed"}
	common.Must(m.AddHandler(context.Background(), handler))

	common.Must(m.RemoveHandler(context.Background(), "removed"))

	if handler.closed != 1 {
		t.Error("expected the removed handler to be closed exactly once, but it was closed", handler.closed, "times")
	}
	if h := m.GetHandler("removed"); h != nil {
		t.Error("expected the handler to be gone from the manager")
	}
}

func TestRemoveUnknownHandler(t *testing.T) {
	m, err := New(context.Background(), &proxyman.OutboundConfig{})
	common.Must(err)

	// removing a tag that was never added must not panic
	common.Must(m.RemoveHandler(context.Background(), "unknown"))
}

// ParseRandomIP must never panic, whatever it is given.
func TestParseRandomIP(t *testing.T) {
	domain := net.DomainAddress("example.com")
	if got := ParseRandomIP(domain, "24"); got != domain {
		t.Error("expected a domain address to be returned unchanged, but got", got)
	}

	addr := net.ParseAddress("192.168.1.1")
	if got := ParseRandomIP(addr, "not-a-cidr"); got != addr {
		t.Error("expected an invalid cidr to leave the address unchanged, but got", got)
	}

	random := ParseRandomIP(addr, "24")
	if !random.Family().IsIP() {
		t.Fatal("expected an ip address, but got", random)
	}
	if ip := random.IP(); len(ip) != net.IPv4len || ip[0] != 192 || ip[1] != 168 || ip[2] != 1 {
		t.Error("expected an address from 192.168.1.0/24, but got", random)
	}
}

// A domain "Via" combined with a CIDR used to panic in ParseRandomIP, the
// domain has to be handled like any other domain instead.
func TestSetOutboundGatewayWithDomainViaCidr(t *testing.T) {
	config := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&stats.Config{}),
			serial.ToTypedMessage(&policy.Config{System: &policy.SystemPolicy{
				Stats: &policy.SystemPolicy_Stats{},
			}}),
		},
	}

	v, err := core.New(config)
	common.Must(err)
	ctx := context.WithValue(context.Background(), xrayKey, v)

	h, err := NewHandler(ctx, &core.OutboundHandlerConfig{
		Tag: "tag",
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{
			Via:     net.NewIPOrDomain(net.DomainAddress("example.com")),
			ViaCidr: "24",
		}),
		ProxySettings: serial.ToTypedMessage(&freedom.Config{FinalRules: []*freedom.FinalRuleConfig{{Action: freedom.RuleAction_Allow}}}),
	})
	common.Must(err)

	ob := &session.Outbound{}
	h.(*Handler).SetOutboundGateway(ctx, ob)

	if ob.Gateway != net.DomainAddress("example.com") {
		t.Error("expected the domain to be used as gateway, but got", ob.Gateway)
	}
}

func TestSelectAfterRemoveHandler(t *testing.T) {
	m, err := New(context.Background(), &proxyman.OutboundConfig{})
	common.Must(err)
	ctx := context.Background()

	common.Must(m.AddHandler(ctx, &closableHandler{tag: "node1"}))

	var _ outbound.HandlerSelector = m
	if tags := m.Select([]string{"node"}); len(tags) != 1 || tags[0] != "node1" {
		t.Fatal("unexpected tags:", tags)
	}

	common.Must(m.RemoveHandler(ctx, "node1"))

	if tags := m.Select([]string{"node"}); len(tags) != 0 {
		t.Error("expected an empty tag list after removal, but got", tags)
	}
}
