package conf_test

import (
	"testing"

	"github.com/xtls/xray-core/common"
	. "github.com/xtls/xray-core/infra/conf"
)

func TestBufferSize(t *testing.T) {
	cases := []struct {
		Input  int32
		Output int32
	}{
		{
			Input:  0,
			Output: 0,
		},
		{
			Input:  -1,
			Output: -1,
		},
		{
			Input:  1,
			Output: 1024,
		},
	}

	for _, c := range cases {
		bs := c.Input
		pConf := Policy{
			BufferSize: &bs,
		}
		p, err := pConf.Build()
		common.Must(err)
		if p.Buffer.Connection != c.Output {
			t.Error("expected buffer size ", c.Output, " but got ", p.Buffer.Connection)
		}
	}
}

// "bufferSize" is in KB and is multiplied into an int32; without a bound the
// multiplication silently wraps around to a negative buffer size.
func TestOverRangeBufferSize(t *testing.T) {
	for _, input := range []int32{2097152, 4194304, 2147483647} {
		bs := input
		pConf := Policy{
			BufferSize: &bs,
		}
		if p, err := pConf.Build(); err == nil {
			t.Fatalf("expected error for bufferSize %d, but got %v", input, p)
		}
	}

	// The largest accepted value still converts without wrapping.
	bs := int32(2097151)
	pConf := Policy{
		BufferSize: &bs,
	}
	p, err := pConf.Build()
	common.Must(err)
	if p.Buffer.Connection <= 0 {
		t.Fatalf("unexpected buffer size: %v", p.Buffer.Connection)
	}
}
