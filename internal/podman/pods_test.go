package podman

import (
	"slices"
	"strings"
	"testing"
)

// The command line of a pod and of the containers that join it, see PLAN.md
// section 5.6. The flags a pod owns are the ones podman refuses on a container
// created with --pod: verified against podman 5.7.0, which answers "network
// cannot be configured when it is shared with a pod" for --publish and "cannot
// set hostname when joining the pod UTS namespace" for --hostname.

func TestPodCreateArgsCarryThePortAndTheCgroup(t *testing.T) {
	args := PodCreateArgs(PodOptions{
		Name:         "kitbash-board-board",
		Labels:       map[string]string{"kitbash.id": "an-id", "kitbash.pod": "kitbash-board-board"},
		Publish:      []PortMapping{{HostPort: 40275, ContainerPort: 8080}},
		CgroupParent: "/kitbash/loki/an-id",
	})
	want := []string{
		"pod", "create", "--name", "kitbash-board-board",
		"--label", "kitbash.id=an-id",
		"--label", "kitbash.pod=kitbash-board-board",
		"--cgroup-parent=/kitbash/loki/an-id",
		"--publish", "127.0.0.1:40275:8080",
	}
	if !slices.Equal(args, want) {
		t.Errorf("pod create is\n  %v\nwant\n  %v", args, want)
	}
}

// A container of a pod says so and publishes nothing of its own: the pod holds
// the network namespace, and the cgroup comes from the pod because pod create
// defaults to --share-parent.
func TestAUnitOfAPodPublishesNothingOfItsOwn(t *testing.T) {
	args := CreateArgs(RunOptions{
		Name:  "kitbash-board-board-web",
		Image: "sha256:abc",
		Pod:   "kitbash-board-board",
		// A caller that sends these anyway must not have them written: podman
		// refuses the command line and the Process does not start.
		Publish:      []PortMapping{{HostPort: 40275, ContainerPort: 8080}},
		CgroupParent: "/kitbash/loki/an-id",
		Memory:       "128m",
		Detach:       true,
		Interactive:  true,
	}, nil)
	if !slices.Contains(args, "--pod") {
		t.Fatalf("the unit is created without --pod: %v", args)
	}
	if slices.Contains(args, "--publish") {
		t.Errorf("the unit publishes a port beside --pod: %v", args)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--cgroup-parent") || arg == "--cgroups=enabled" {
			t.Errorf("the unit names a cgroup beside --pod: %v", args)
		}
	}
	// What is per unit is still on the line: the pod is the ceiling and each
	// unit carries its own limits under it.
	if !slices.Contains(args, "--memory") {
		t.Errorf("the unit was created without its own limits: %v", args)
	}
}

// A container that is not in a pod is created by exactly the command line it
// always was, which is what keeps every Package of one unit on a kitbash host
// the Process it is.
func TestABareContainerIsUnchangedByPods(t *testing.T) {
	args := CreateArgs(RunOptions{
		Name:         "kitbash-echo-echo",
		Image:        "sha256:abc",
		Publish:      []PortMapping{{HostPort: 40275, ContainerPort: 8080}},
		CgroupParent: "/kitbash/loki/an-id",
		Detach:       true,
		Interactive:  true,
	}, nil)
	want := []string{
		"create", "--name", "kitbash-echo-echo", "--interactive",
		"--cgroups=enabled", "--cgroup-parent=/kitbash/loki/an-id",
		"--publish", "127.0.0.1:40275:8080",
		"sha256:abc",
	}
	if !slices.Equal(args, want) {
		t.Errorf("a bare container is created with\n  %v\nwant\n  %v", args, want)
	}
}
