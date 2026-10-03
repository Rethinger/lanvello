package keys_test

import (
	"testing"

	"lanvello/internal/keys"
)

func TestAddVerifyRevoke(t *testing.T) {
	dir := t.TempDir()
	s, err := keys.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, _, err := s.Add("aider")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Verify(sec, false) {
		t.Fatal("verify failed")
	}
	if s.Verify("sk-lanv-bogus", false) {
		t.Fatal("bogus verified")
	}
	if !s.Revoke("aider") {
		t.Fatal("revoke failed")
	}
	if s.Verify(sec, false) {
		t.Fatal("revoked still verifies")
	}
}
