package telemetry

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// What goes into the server log from a body nobody structured is bounded: the
// answer could be a megabyte of HTML from something that is not kitbashd at
// all.
func TestClipBoundsWhatIsCopiedOutOfABody(t *testing.T) {
	long := strings.Repeat("x", 4096)

	got := clip(long, maxDetailBytes)
	if !strings.HasPrefix(got, strings.Repeat("x", maxDetailBytes)) {
		t.Errorf("the clipped body does not start with the body: %q", got[:64])
	}
	if !strings.Contains(got, "3072 bytes truncated") {
		t.Errorf("the clipped body does not say how much it dropped: %q", got[maxDetailBytes:])
	}
	if len(got) > maxDetailBytes+64 {
		t.Errorf("the clipped body is %d bytes, want about %d", len(got), maxDetailBytes)
	}
}

func TestClipLeavesAShortBodyAlone(t *testing.T) {
	short := "kitbashd is not this"
	if got := clip(short, maxDetailBytes); got != short {
		t.Errorf("clip returned %q, want the body unchanged", got)
	}
}

// The budget is bytes and a body is not always ASCII, so the cut moves back
// off a partial rune rather than leaving one behind.
func TestClipCutsOnARuneBoundary(t *testing.T) {
	// Three byte runes, so a cut at 10 bytes falls inside one.
	body := strings.Repeat("空", 8)

	got := clip(body, 10)
	if !utf8.ValidString(got) {
		t.Errorf("the clipped body is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("空", 3)) {
		t.Errorf("the clipped body is %q, want the runes that fit", got)
	}
}
