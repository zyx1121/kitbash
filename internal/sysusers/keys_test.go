package sysusers_test

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/sysusers"
)

// blob builds an OpenSSH public key body: the type as a length prefixed
// string, then the key material. Generating one here rather than pasting a
// real key keeps the test readable and says what the format is.
func blob(keyType string, material int) string {
	out := make([]byte, 0, 4+len(keyType)+4+material)
	out = binary.BigEndian.AppendUint32(out, uint32(len(keyType)))
	out = append(out, keyType...)
	out = binary.BigEndian.AppendUint32(out, uint32(material))
	for i := range material {
		out = append(out, byte(i))
	}
	return base64.StdEncoding.EncodeToString(out)
}

// key is one well formed key line of the given type.
func key(keyType string) string { return keyType + " " + blob(keyType, 32) }

func TestValidateKeyAccepts(t *testing.T) {
	cases := map[string]struct {
		line string
		want string
	}{
		"ed25519 with a comment": {
			line: key("ssh-ed25519") + " loki@macbook",
			want: key("ssh-ed25519") + " loki@macbook",
		},
		"no comment": {
			line: key("ssh-rsa"),
			want: key("ssh-rsa"),
		},
		"a trailing newline is not a second line": {
			line: key("ssh-ed25519") + "\n",
			want: key("ssh-ed25519"),
		},
		"extra spacing is normalised": {
			line: "  " + "ssh-ed25519" + "   " + blob("ssh-ed25519", 32) + "   loki  laptop  ",
			want: key("ssh-ed25519") + " loki laptop",
		},
		"a hardware key type": {
			line: key("sk-ssh-ed25519@openssh.com"),
			want: key("sk-ssh-ed25519@openssh.com"),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := sysusers.ValidateKey(c.line)
			if err != nil {
				t.Fatalf("ValidateKey: %v", err)
			}
			if got != c.want {
				t.Errorf("key = %q, want %q", got, c.want)
			}
		})
	}
}

// TestValidateKeyRefuses is the rule authorized_keys rests on: a line that is
// not a key is never written, because sshd reads that file as authority and an
// option string in it changes what every key below it may do.
func TestValidateKeyRefuses(t *testing.T) {
	cases := map[string]string{
		"empty":                     "",
		"only a type":               "ssh-ed25519",
		"an unknown type":           "ssh-dss " + blob("ssh-dss", 32),
		"a body that is not base64": "ssh-ed25519 not-base64!!",
		"a body of another type":    "ssh-ed25519 " + blob("ssh-rsa", 32),
		"two lines": key("ssh-ed25519") + "\n" +
			`command="rm -rf /" ` + key("ssh-ed25519"),
		"an options prefix":   `no-pty ` + key("ssh-ed25519"),
		"a control character": "ssh-ed25519\x00 " + blob("ssh-ed25519", 32),
		// A byte under 0x20 inside a sequence that is not valid UTF-8 would
		// decode as one replacement rune, so the check is byte by byte.
		"a control byte in bad UTF-8": "ssh-ed25519 \xff\x01" + blob("ssh-ed25519", 32),
		"a delete byte":               "ssh-ed25519 \x7f" + blob("ssh-ed25519", 32),
		"a body that is empty":        "ssh-ed25519 ",
		"a truncated blob":            "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte{0, 0}),
		"over the size a key carries": key("ssh-ed25519") + " " + strings.Repeat("a", sysusers.MaxKeyBytes),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := sysusers.ValidateKey(line)
			if err == nil {
				t.Fatalf("ValidateKey(%q) = %q, want a refusal", line, got)
			}
			if !errors.Is(err, sysusers.ErrKey) {
				t.Errorf("error = %v, want ErrKey", err)
			}
		})
	}
}
