package echo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ZacxDev/muster-plugin-template/internal/echo"
)

// The cursor survives a restart: a new process on the same state directory
// starts after the last message muster accepted, and Next alone never moves it.
func TestTheCursorSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	u, err := echo.OpenUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := u.Next().ExternalID, u.Next().ExternalID; a != "echo-1" || b != "echo-1" {
		t.Fatalf("Next without Advance = %s then %s, want echo-1 twice", a, b)
	}
	for range 2 {
		if err := u.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	if got := u.Next().ExternalID; got != "echo-3" {
		t.Fatalf("after two Advances Next = %s, want echo-3", got)
	}

	again, err := echo.OpenUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Next().ExternalID; got != "echo-3" {
		t.Fatalf("restart: Next = %s, want echo-3 (the cursor was not kept in STATE_DIR)", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, echo.CursorFile)); err != nil || string(b) != "2\n" {
		t.Fatalf("cursor file = %q, %v; want \"2\\n\"", b, err)
	}
}

func TestAnUnreadableCursorStopsTheStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, echo.CursorFile), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := echo.OpenUpstream(dir); err == nil {
		t.Fatal("OpenUpstream accepted an unreadable cursor")
	}
}
