package uuid_test

import (
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/uuid"
)

var canonical = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestV7IsCanonicalAndVersioned(t *testing.T) {
	id := uuid.V7()
	if !canonical.MatchString(id) {
		t.Fatalf("id is %q, want a canonical version 7 UUID", id)
	}
}

func TestV7IsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := uuid.V7()
		if seen[id] {
			t.Fatalf("id %s was generated twice", id)
		}
		seen[id] = true
	}
}

// Version 7 sorts by its millisecond field, so ids from the same millisecond
// have no defined order and the test crosses one every time.
func TestV7SortsByTime(t *testing.T) {
	var ids []string
	for i := 0; i < 8; i++ {
		ids = append(ids, uuid.V7())
		time.Sleep(2 * time.Millisecond)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("id %d is out of order: %s came before %s", i, ids[i], sorted[i])
		}
	}
}
