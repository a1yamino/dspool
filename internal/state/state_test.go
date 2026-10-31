package state

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTombstonesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "state.json")
	st, err := Open(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if st.IsInvalid("abc") {
		t.Fatal("unexpected invalid key")
	}
	now := time.Now().UTC().Truncate(time.Second)
	st.MarkInvalid("abc", "label", "sk-…1234", "401 invalid", now)
	if !st.IsInvalid("abc") {
		t.Fatal("tombstone was not recorded")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file permissions = %o, want private", info.Mode().Perm())
	}

	reopened, err := Open(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.IsInvalid("abc") {
		t.Fatal("tombstone did not survive reopen")
	}
	entry := reopened.InvalidKeys()["abc"]
	if entry.Label != "label" || entry.Reason != "401 invalid" {
		t.Fatalf("entry = %+v", entry)
	}

	reopened.ClearInvalid("abc")
	if reopened.IsInvalid("abc") {
		t.Fatal("tombstone was not cleared")
	}
	again, err := Open(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if again.IsInvalid("abc") {
		t.Fatal("cleared tombstone survived reopen")
	}
}

func TestCorruptStateFileFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, testLogger()); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestInvalidKeysReturnsCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	st.MarkInvalid("x", "l", "m", "r", time.Now())
	copy1 := st.InvalidKeys()
	delete(copy1, "x")
	if !st.IsInvalid("x") {
		t.Fatal("InvalidKeys leaked internal map")
	}
}
