package conf_test

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/app/commander"
	handlerservice "github.com/xtls/xray-core/app/proxyman/command"
	statsservice "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/serial"
	. "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

func TestAPIConfig(t *testing.T) {
	// APIConfig.Build returns a concrete type, so it is not a Buildable.
	parser := func(s string) (proto.Message, error) {
		instance := new(APIConfig)
		if err := json.Unmarshal([]byte(s), instance); err != nil {
			return nil, err
		}
		return instance.Build()
	}

	config, err := parser(`{"tag": "api", "services": ["HandlerService", "StatsService"]}`)
	common.Must(err)

	expected := &commander.Config{
		Tag: "api",
		Service: []*serial.TypedMessage{
			serial.ToTypedMessage(&handlerservice.Config{}),
			serial.ToTypedMessage(&statsservice.Config{}),
		},
	}
	if !proto.Equal(expected, config.(*commander.Config)) {
		t.Fatalf("unexpected API config: %v", config)
	}

	// An unknown service is a typo, not a silent no-op.
	for _, raw := range []string{
		`{"tag": "api", "services": ["handlerservic"]}`,
		`{"tag": "api", "services": ["Stats", "HandlerService"]}`,
		`{"tag": "api", "services": [""]}`,
	} {
		if config, err := parser(raw); err == nil {
			t.Fatalf("expected error for %s, but got %v", raw, config)
		}
	}

	if config, err := parser(`{"services": ["HandlerService"]}`); err == nil {
		t.Fatalf("expected error for an empty tag, but got %v", config)
	}
}
