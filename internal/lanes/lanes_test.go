package lanes_test

import (
	"testing"

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
