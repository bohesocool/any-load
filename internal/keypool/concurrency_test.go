package keypool

import (
	"testing"

	"any-load/internal/store"
)

// newTestProvider builds a KeyProvider backed by an in-memory store. Only the
// store is needed for AcquireKey/ReleaseKey; the other dependencies are nil.
func newTestProvider() *KeyProvider {
	return &KeyProvider{store: store.NewMemoryStore()}
}

func TestAcquireReleaseUnlimited(t *testing.T) {
	p := newTestProvider()
	const gid, kid uint = 1, 10
	// maxConc <= 0 means unlimited: no store traffic, always succeeds.
	for i := 0; i < 5; i++ {
		ok, err := p.AcquireKey(gid, kid, 0)
		if err != nil || !ok {
			t.Fatalf("acquire unlimited failed: ok=%v err=%v", ok, err)
		}
	}
	// Release is a no-op; the inflight hash must not have been written.
	if cnt, _ := p.store.HGetAll(inflightHashKey(gid)); len(cnt) != 0 {
		t.Fatalf("unlimited mode wrote inflight: %v", cnt)
	}
	p.ReleaseKey(gid, kid, 0)
}

func TestAcquireRespectsLimit(t *testing.T) {
	p := newTestProvider()
	const gid, kid uint = 2, 20

	// Limit of 2: two acquires succeed, the third is rejected.
	if ok, err := p.AcquireKey(gid, kid, 2); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := p.AcquireKey(gid, kid, 2); err != nil || !ok {
		t.Fatalf("second acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := p.AcquireKey(gid, kid, 2); err != nil || ok {
		t.Fatalf("third acquire should be rejected: ok=%v err=%v", ok, err)
	}

	// After a release, one slot frees up and acquire succeeds again.
	p.ReleaseKey(gid, kid, 2)
	if ok, err := p.AcquireKey(gid, kid, 2); err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}

	// Count must be back at the limit (2), not above.
	details, _ := p.store.HGetAll(inflightHashKey(gid))
	if v := details["20"]; v != "2" {
		t.Fatalf("inflight count = %q, want 2", v)
	}
}

func TestReleaseFloorsAtZero(t *testing.T) {
	p := newTestProvider()
	const gid, kid uint = 3, 30
	// Acquire once, release twice: must not go negative.
	p.AcquireKey(gid, kid, 1)
	p.ReleaseKey(gid, kid, 1)
	p.ReleaseKey(gid, kid, 1) // stray release
	details, _ := p.store.HGetAll(inflightHashKey(gid))
	if v := details["30"]; v != "0" {
		t.Fatalf("inflight after over-release = %q, want 0", v)
	}
	// A subsequent acquire must still succeed.
	if ok, err := p.AcquireKey(gid, kid, 1); err != nil || !ok {
		t.Fatalf("acquire after over-release: ok=%v err=%v", ok, err)
	}
}

func TestAcquireIsPerKey(t *testing.T) {
	p := newTestProvider()
	const gid uint = 4
	// Key A at capacity must not block key B.
	if ok, _ := p.AcquireKey(gid, 100, 1); !ok {
		t.Fatal("key A first acquire failed")
	}
	if ok, _ := p.AcquireKey(gid, 100, 1); ok {
		t.Fatal("key A second acquire should be rejected")
	}
	if ok, _ := p.AcquireKey(gid, 200, 1); !ok {
		t.Fatal("key B acquire should be independent of key A")
	}
}
