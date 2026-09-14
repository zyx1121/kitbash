package podman

import (
	"strings"
	"testing"
)

// The command line is the whole of what this package decides about a mount, so
// the exact argv is the assertion. The option spellings below were verified
// against podman 5.7.0 on a kitbash host: the mount lands in the container's
// /proc/self/mountinfo as ro,nosuid,nodev,noexec, and podman refuses an option
// it does not know rather than dropping it.

func TestMountSpecIsTheOptionsPodmanTakes(t *testing.T) {
	for _, c := range []struct {
		name  string
		mount Mount
		want  string
	}{
		{
			name:  "read only",
			mount: Mount{Source: "/home/loki/notes", Target: "/files/notes", ReadOnly: true},
			want:  "type=bind,src=/home/loki/notes,dst=/files/notes,ro=true,bind-nonrecursive,nosuid,nodev,noexec",
		},
		{
			name:  "read write",
			mount: Mount{Source: "/home/loki/out", Target: "/files/out"},
			want:  "type=bind,src=/home/loki/out,dst=/files/out,ro=false,bind-nonrecursive,nosuid,nodev,noexec",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := MountSpec(c.mount); got != c.want {
				t.Errorf("MountSpec is\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// Every mount is one --mount flag, in the order the unit declared them, and
// they sit before the image like the rest of the run flags: podman stops
// parsing flags at the image name.
func TestRunArgsCarriesEveryMountBeforeTheImage(t *testing.T) {
	args := RunArgs(RunOptions{
		Name:        "kitbash-reader-reader",
		Image:       "sha256:abc",
		Detach:      true,
		Interactive: true,
		Mounts: []Mount{
			{Source: "/home/loki/notes", Target: "/files/notes", ReadOnly: true},
			{Source: "/org/handbook", Target: "/files/handbook", ReadOnly: true},
		},
	}, nil)
	want := []string{
		"run", "--name", "kitbash-reader-reader", "--detach", "--interactive",
		"--mount", "type=bind,src=/home/loki/notes,dst=/files/notes,ro=true,bind-nonrecursive,nosuid,nodev,noexec",
		"--mount", "type=bind,src=/org/handbook,dst=/files/handbook,ro=true,bind-nonrecursive,nosuid,nodev,noexec",
		"sha256:abc",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("RunArgs is\n%s\nwant\n%s", strings.Join(args, " "), strings.Join(want, " "))
	}
}

// A unit that declares no mount produces the command line it produced before
// mounts existed, which is what keeps every Process already registered running
// the same command after an upgrade.
func TestRunArgsCarriesNoMountFlagWithoutMounts(t *testing.T) {
	args := RunArgs(RunOptions{Name: "kitbash-echo-echo", Image: "sha256:abc"}, nil)
	for _, arg := range args {
		if arg == "--mount" {
			t.Fatalf("RunArgs is %v, want no mount flag", args)
		}
	}
}
