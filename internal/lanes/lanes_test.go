package lanes_test

import (
	"testing"
	"time"

	"lanvello/internal/lanes"
)

func TestPickLeastLoaded(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"de", "nl"}, nil, []string{"127.0.0.1:1111", "127.0.0.1:2222"}, true, 2)
	a := m.Pick(nil)
	if a == nil {
		t.Fatal("no lane")
	}
	m.Release(a)
	b := m.Pick(map[int]bool{a.Index: true})
	if b == nil || b.Index == a.Index {
		t.Fatal("exclude ignored")
	}
	m.Release(b)
}

func TestPickCountry(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"us", "de"}, nil, []string{"127.0.0.1:1111", "127.0.0.1:2222"}, true, 2)
	got := m.PickCountry(nil, "de")
	if got == nil || got.Country != "de" {
		t.Fatalf("want de lane, got %+v", got)
	}
	m.Release(got)
	got = m.PickCountry(nil, "fr")
	if got == nil {
		t.Fatal("fallback failed")
	}
	m.Release(got)
}

func TestLimitedLanesDeprioritized(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"de", "nl"}, nil, []string{"127.0.0.1:1111", "127.0.0.1:2222"}, true, 2)
	ls := m.Lanes()
	m.NoteLimited(ls[0], 0)
	got := m.Pick(nil)
	if got == nil || got.Index != ls[1].Index {
		t.Fatalf("limited lane picked: %+v", got)
	}
	m.Release(got)
}

// A 429 rotation must not hand the same limited exit back: the retry-after
// limit stays until the respawned exit is up.
func TestRotateKeepsRetryAfter(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"de", "nl"}, nil, []string{"127.0.0.1:1111", "127.0.0.1:2222"}, true, 2)
	ls := m.Lanes()
	m.NoteLimited(ls[0], 10*time.Minute)
	if ls[0].LimitedTill.IsZero() {
		t.Fatal("limit not set")
	}
	m.Rotate(ls[0])
	if ls[0].LimitedTill.IsZero() {
		t.Fatal("rotation cleared the retry-after limit")
	}
	got := m.Pick(nil)
	if got == nil || got.Index == ls[0].Index {
		t.Fatalf("limited lane picked after rotate: %+v", got)
	}
	m.Release(got)
}

func TestRotateHookAndDone(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"de", "nl"}, nil, []string{"127.0.0.1:1111", "127.0.0.1:2222"}, true, 2)
	ls := m.Lanes()
	called := make(chan int, 1)
	m.OnRotate = func(l *lanes.Lane) { called <- l.Index }
	m.NoteLimited(ls[0], 0)
	m.Rotate(ls[0])
	select {
	case idx := <-called:
		if idx != ls[0].Index {
			t.Fatalf("hook lane=%d", idx)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRotate hook never fired")
	}
	// while rotating, the lane is out of the pool
	if got := m.Pick(map[int]bool{ls[1].Index: true}); got != nil {
		t.Fatalf("rotating lane picked: %+v", got)
	}
	m.RotateDone(ls[0], "127.0.0.1:3333", "203.0.113.7")
	if ls[0].Rotating || !ls[0].LimitedTill.IsZero() || ls[0].SocksAddr != "127.0.0.1:3333" {
		t.Fatalf("RotateDone not applied: %+v", ls[0])
	}
}
