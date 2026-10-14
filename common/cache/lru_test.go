package cache_test

import (
	"testing"

	. "github.com/xtls/xray-core/common/cache"
)

func TestLruReplaceValue(t *testing.T) {
	lru := NewLru(2)
	lru.Put(2, 6)
	lru.Put(1, 5)
	lru.Put(1, 2)
	v, _ := lru.Get(1)
	if v != 2 {
		t.Error("should get 2", v)
	}
	v, _ = lru.Get(2)
	if v != 6 {
		t.Error("should get 6", v)
	}
}

func TestLruRemoveOld(t *testing.T) {
	lru := NewLru(2)
	v, ok := lru.Get(2)
	if ok {
		t.Error("should get nil", v)
	}
	lru.Put(1, 1)
	lru.Put(2, 2)
	v, _ = lru.Get(1)
	if v != 1 {
		t.Error("should get 1", v)
	}
	lru.Put(3, 3)
	v, ok = lru.Get(2)
	if ok {
		t.Error("should get nil", v)
	}
	lru.Put(4, 4)
	v, ok = lru.Get(1)
	if ok {
		t.Error("should get nil", v)
	}
	v, _ = lru.Get(3)
	if v != 3 {
		t.Error("should get 3", v)
	}
	v, _ = lru.Get(4)
	if v != 4 {
		t.Error("should get 4", v)
	}
}

func TestGetKeyFromValue(t *testing.T) {
	lru := NewLru(2)
	lru.Put(3, 3)
	lru.Put(2, 2)
	lru.GetKeyFromValue(3)
	lru.Put(1, 1)
	v, ok := lru.GetKeyFromValue(2)
	if ok {
		t.Error("should get nil", v)
	}
	v, _ = lru.GetKeyFromValue(3)
	if v != 3 {
		t.Error("should get 3", v)
	}
}

func TestPeekKeyFromValue(t *testing.T) {
	lru := NewLru(2)
	lru.Put(3, 3)
	lru.Put(2, 2)
	lru.PeekKeyFromValue(3)
	lru.Put(1, 1)
	v, ok := lru.PeekKeyFromValue(3)
	if ok {
		t.Error("should get nil", v)
	}
	v, _ = lru.PeekKeyFromValue(2)
	if v != 2 {
		t.Error("should get 2", v)
	}
}

// Replacing the value of an existing key used to leave the old value in the
// reverse index, so the cache grew without bound while holding a single key.
func TestLruReplaceValueDropsOldValueIndex(t *testing.T) {
	lru := NewLru(2)
	lru.Put(1, 1)

	for i := 2; i <= 1000; i++ {
		lru.Put(1, i)
		if v, ok := lru.GetKeyFromValue(i - 1); ok {
			t.Fatalf("stale value %d is still indexed to key %v", i-1, v)
		}
		if v, ok := lru.PeekKeyFromValue(i - 1); ok {
			t.Fatalf("stale value %d is still peekable to key %v", i-1, v)
		}
	}

	if v, ok := lru.Get(1); !ok || v != 1000 {
		t.Fatalf("Get(1) = %v, %v, want 1000, true", v, ok)
	}
	if k, ok := lru.GetKeyFromValue(1000); !ok || k != 1 {
		t.Fatalf("GetKeyFromValue(1000) = %v, %v, want 1, true", k, ok)
	}

	// The key stays put, so entries that do not fit are still evicted.
	lru.Put(2, 2000)
	lru.Put(3, 3000)
	if v, ok := lru.Get(1); ok {
		t.Fatal("expected key 1 to be evicted, got ", v)
	}
}

// Putting the very same value again must keep it resolvable.
func TestLruPutSameValue(t *testing.T) {
	lru := NewLru(2)
	lru.Put(1, 1)
	lru.Put(1, 1)

	if k, ok := lru.GetKeyFromValue(1); !ok || k != 1 {
		t.Fatalf("GetKeyFromValue(1) = %v, %v, want 1, true", k, ok)
	}
	if v, ok := lru.Get(1); !ok || v != 1 {
		t.Fatalf("Get(1) = %v, %v, want 1, true", v, ok)
	}
}
