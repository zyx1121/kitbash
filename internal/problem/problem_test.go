package problem_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
)

func TestInternalHidesTheCause(t *testing.T) {
	cause := "git commit: exit status 128: fatal: unable to access /org/handbook/.git"
	p := problem.Internal("/org/handbook/README.md", cause, "")
	if strings.Contains(p.Detail, cause) {
		t.Errorf("the cause reached the agent: %q", p.Detail)
	}
	if strings.Contains(p.JSON(), "exit status 128") {
		t.Error("git output reached the wire")
	}
	if p.Status != 500 || p.Slug() != problem.SlugInternal {
		t.Errorf("problem is %+v", p)
	}
}

func TestJSONCarriesTheRequiredMembers(t *testing.T) {
	p := problem.NotFound("/org/handbook/missing.md", "no such file or folder")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(p.JSON()), &decoded); err != nil {
		t.Fatalf("problem is not JSON: %v", err)
	}
	for _, key := range []string{"type", "title", "status", "detail", "instance", "fix"} {
		if _, found := decoded[key]; !found {
			t.Errorf("%s is missing from %v", key, decoded)
		}
	}
	if want := problem.Base + problem.SlugNotFound; decoded["type"] != want {
		t.Errorf("type is %v, want %s", decoded["type"], want)
	}
}
