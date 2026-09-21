package podman

import (
	"strings"
	"testing"
)

// A unit's command is what podman reads after the image, so the exact argv is
// the assertion: a word before the image would be parsed as a flag of podman's
// own, and the container would run the command of its image instead.
func TestRunArgsCarriesTheCommandAfterTheImage(t *testing.T) {
	args := RunArgs(RunOptions{
		Name:        "kitbash-board-board",
		Image:       "sha256:abc",
		Detach:      true,
		Interactive: true,
		Publish:     []PortMapping{{HostPort: 40275, ContainerPort: 8080}},
		Command:     []string{"redis-server", "--save", ""},
	}, nil)
	want := []string{
		"run", "--name", "kitbash-board-board", "--detach", "--interactive",
		"--publish", "127.0.0.1:40275:8080",
		"sha256:abc",
		"redis-server", "--save", "",
	}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("RunArgs is\n%s\nwant\n%s", strings.Join(args, "|"), strings.Join(want, "|"))
	}
}

// podman create takes the same words in the same place, which is what the four
// step start of PLAN.md section 2.3 uses.
func TestCreateArgsCarriesTheCommandAfterTheImage(t *testing.T) {
	args := CreateArgs(RunOptions{
		Name:    "kitbash-board-board",
		Image:   "sha256:abc",
		Command: []string{"node", "server.js"},
	}, nil)
	if got := strings.Join(args[len(args)-3:], " "); got != "sha256:abc node server.js" {
		t.Errorf("CreateArgs ends with %q, want the image and then the command", got)
	}
}

// A unit that declares no command produces the command line it produced
// before commands existed, so the image decides what runs.
func TestRunArgsCarriesNothingAfterTheImageWithoutACommand(t *testing.T) {
	args := RunArgs(RunOptions{Name: "kitbash-echo-echo", Image: "sha256:abc"}, nil)
	if args[len(args)-1] != "sha256:abc" {
		t.Errorf("RunArgs ends with %q, want the image", args[len(args)-1])
	}
}
