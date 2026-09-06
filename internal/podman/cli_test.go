package podman

import (
	"encoding/json"
	"strings"
	"testing"
)

// The runtime is exercised end to end on a real host, where podman exists.
// What is worth testing here is the decoding of what podman prints, which is
// where a version difference shows up first.

func TestContainerJSONDecodesPodmanPS(t *testing.T) {
	const out = `[{"Id":"9a8b","Names":["kitbash-ffmpeg-ffmpeg"],"Image":"sha256:abc",
"State":"running","ExitCode":0,"StartedAt":1767225600,
"Labels":{"kitbash.id":"0192f000-0000-7000-8000-000000000000","kitbash.user":"tester"},
"Ports":[{"host_ip":"127.0.0.1","container_port":8080,"host_port":34567,"protocol":"tcp"}]}]`

	var decoded []containerJSON
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decoding podman ps: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded %d containers, want 1", len(decoded))
	}
	got := decoded[0]
	if got.Names[0] != "kitbash-ffmpeg-ffmpeg" || got.State != "running" {
		t.Errorf("container is %+v, want the running kitbash container", got)
	}
	if got.Labels[LabelUser] != "tester" {
		t.Errorf("labels are %v, want the kitbash.user label", got.Labels)
	}
	if len(got.Ports) != 1 || got.Ports[0].HostPort != 34567 || got.Ports[0].ContainerPort != 8080 {
		t.Errorf("ports are %+v, want 8080 published on 34567", got.Ports)
	}
}

func TestInspectJSONDecodesBothEntrypointForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{name: "array", body: `[{"Config":{"Entrypoint":["/bin/server","--stdio"]}}]`,
			want: []string{"/bin/server", "--stdio"}},
		{name: "string", body: `[{"Config":{"Entrypoint":"/bin/server"}}]`,
			want: []string{"/bin/server"}},
		{name: "absent", body: `[{"Config":{"Cmd":["/bin/server"]}}]`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded []inspectJSON
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				t.Fatalf("decoding podman image inspect: %v", err)
			}
			got := strings.Join(decoded[0].Config.Entrypoint, " ")
			if got != strings.Join(tc.want, " ") {
				t.Errorf("entrypoint is %q, want %q", got, strings.Join(tc.want, " "))
			}
		})
	}
}

func TestNormalizeID(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := normalizeID(hex); got != "sha256:"+hex {
		t.Errorf("normalizeID is %q, want the sha256 prefix", got)
	}
	if got := normalizeID("sha256:" + hex); got != "sha256:"+hex {
		t.Errorf("normalizeID doubled the prefix: %q", got)
	}
	if got := normalizeID(""); got != "" {
		t.Errorf("normalizeID of nothing is %q, want nothing", got)
	}
}

func TestSplitLines(t *testing.T) {
	if got := SplitLines("one\ntwo\n"); len(got) != 2 || got[1] != "two" {
		t.Errorf("SplitLines is %v, want two lines", got)
	}
	if got := SplitLines(""); len(got) != 0 {
		t.Errorf("SplitLines of nothing is %v, want no lines", got)
	}
}

// The double dash keeps an entrypoint that takes its own flags out of podman's
// option parsing.
func TestExecArgvHoldsStdinOpen(t *testing.T) {
	argv := (&CLI{}).ExecArgv("kitbash-ffmpeg-ffmpeg", []string{"/bin/server", "--stdio"})
	want := []string{Binary, "exec", "--interactive", "kitbash-ffmpeg-ffmpeg", "--", "/bin/server", "--stdio"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("ExecArgv is %v, want %v", argv, want)
	}
}
