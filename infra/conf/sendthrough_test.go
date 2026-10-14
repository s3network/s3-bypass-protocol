package conf_test

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	. "github.com/xtls/xray-core/infra/conf"
)

func buildOutboundWithSendThrough(t *testing.T, sendThrough string) (*proxyman.SenderConfig, error) {
	t.Helper()

	raw, err := json.Marshal(map[string]interface{}{
		"protocol":    "freedom",
		"sendThrough": sendThrough,
		"tag":         "out",
	})
	if err != nil {
		t.Fatal(err)
	}

	outbound := new(OutboundDetourConfig)
	if err := json.Unmarshal(raw, outbound); err != nil {
		t.Fatal(err)
	}

	config, err := outbound.Build()
	if err != nil {
		return nil, err
	}

	instance, err := config.SenderSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	sender, ok := instance.(*proxyman.SenderConfig)
	if !ok {
		t.Fatalf("unexpected proxy settings: %T", instance)
	}
	return sender, nil
}

func TestSendThrough(t *testing.T) {
	cases := []struct {
		sendThrough string
		viaCidr     string
	}{
		{sendThrough: "10.0.0.1"},
		{sendThrough: "10.0.0.1/24", viaCidr: "24"},
		{sendThrough: "2001:db8::1"},
		{sendThrough: "2001:db8::/32", viaCidr: "32"},
		{sendThrough: "origin"},
		{sendThrough: "srcip"},
	}

	for _, test := range cases {
		sender, err := buildOutboundWithSendThrough(t, test.sendThrough)
		if err != nil {
			t.Errorf("sendThrough %q: unexpected error: %v", test.sendThrough, err)
			continue
		}
		if sender.ViaCidr != test.viaCidr {
			t.Errorf("sendThrough %q: ViaCidr = %q, want %q", test.sendThrough, sender.ViaCidr, test.viaCidr)
		}
		if sender.Via == nil {
			t.Errorf("sendThrough %q: Via is empty", test.sendThrough)
		}
	}
}

// A "/" used to skip the domain check, so a domain or a bogus CIDR slipped
// through into the sender settings.
func TestSendThroughInvalid(t *testing.T) {
	for _, sendThrough := range []string{
		"evil.example.com",
		"evil.example.com/24",
		"1.2.3.4/не-cidr",
		"1.2.3.4/33",
		"1.2.3.4/24/8",
		"evil.example.com/не-cidr",
		"/24",
	} {
		if sender, err := buildOutboundWithSendThrough(t, sendThrough); err == nil {
			t.Errorf("sendThrough %q: expected error, but got %v", sendThrough, sender)
		}
	}
}
