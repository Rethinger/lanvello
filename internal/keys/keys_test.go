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

// The running server must see keys added or revoked by the CLI (`lanvello
// key add`, a separate process) without a restart.
func TestStorePicksUpExternalChanges(t *testing.T) {
	dir := t.TempDir()
	srv, err := keys.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := keys.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, _, err := cli.Add("atractio")
	if err != nil {
		t.Fatal(err)
	}
	if !srv.Verify(sec, false) {
		t.Fatal("a key added by another process must verify without a restart")
	}
	var names []string
	for _, e := range srv.List() {
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "atractio" {
		t.Fatalf("list=%v", names)
	}
	if !cli.Revoke("atractio") {
		t.Fatal("revoke failed")
	}
	if srv.Verify(sec, false) {
		t.Fatal("a key revoked by another process must stop verifying")
	}
}
