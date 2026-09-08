package sysusers

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// MaxKeyBytes is the longest key line kitbash writes, the same bound
// spec/mcp-surface.yaml puts on the sshKey field.
const MaxKeyBytes = 4096

// KeyTypes are the OpenSSH key types kitbash writes into an authorized_keys.
// ssh-dss is deliberately absent: OpenSSH has not enabled it by default for
// years and a member whose only key is a DSA key could not connect anyway.
var KeyTypes = []string{
	"ssh-ed25519",
	"ssh-rsa",
	"ecdsa-sha2-nistp256",
	"ecdsa-sha2-nistp384",
	"ecdsa-sha2-nistp521",
	"sk-ssh-ed25519@openssh.com",
	"sk-ecdsa-sha2-nistp256@openssh.com",
}

// ValidateKey checks one OpenSSH public key line and returns it in the form
// kitbash writes: type, body and the comment if there was one, separated by
// single spaces and with no trailing newline.
//
// A key that fails here is never written. authorized_keys is the whole
// authentication story for a member, see PLAN.md section 4.5, so a line that
// is not a key is a line that either locks the member out or, worse, is an
// option string that changes what every key below it may do.
func ValidateKey(line string) (string, error) {
	if len(line) > MaxKeyBytes {
		return "", fmt.Errorf("%w: the line is %d bytes, over the %d one key may carry",
			ErrKey, len(line), MaxKeyBytes)
	}
	trimmed := strings.Trim(line, " \t\r\n")
	if trimmed == "" {
		return "", fmt.Errorf("%w: the line is empty", ErrKey)
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return "", fmt.Errorf("%w: a key is one line and this is more than one", ErrKey)
	}
	// A NUL or any other control byte in a file sshd parses line by line is
	// never anything an honest key generator wrote.
	for _, r := range trimmed {
		if r < 0x20 && r != '\t' {
			return "", fmt.Errorf("%w: the line carries a control character", ErrKey)
		}
	}

	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return "", fmt.Errorf("%w: a key is a type, a base64 body and an optional comment", ErrKey)
	}
	keyType, body := fields[0], fields[1]
	if !knownType(keyType) {
		return "", fmt.Errorf("%w: %q is not a key type kitbash accepts", ErrKey, keyType)
	}
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("%w: the body is not base64", ErrKey)
	}
	// The wire format names the type again in its first string. A blob whose
	// name disagrees with the line's type is a line assembled by hand.
	if name, ok := firstString(blob); !ok || name != keyType {
		return "", fmt.Errorf("%w: the body is not a %s public key", ErrKey, keyType)
	}

	out := keyType + " " + body
	if comment := strings.Join(fields[2:], " "); comment != "" {
		out += " " + comment
	}
	return out, nil
}

// knownType reports whether the line names a key type kitbash writes.
func knownType(t string) bool {
	for _, known := range KeyTypes {
		if t == known {
			return true
		}
	}
	return false
}

// firstString reads the leading length prefixed string of an OpenSSH blob,
// which is the key type the generator stamped into it.
func firstString(blob []byte) (string, bool) {
	if len(blob) < 4 {
		return "", false
	}
	n := binary.BigEndian.Uint32(blob[:4])
	if n == 0 || n > uint32(len(blob)-4) || n > MaxKeyBytes {
		return "", false
	}
	return string(blob[4 : 4+n]), true
}

// keyLines splits an authorized_keys file into the key lines it holds,
// ignoring blank lines and comments, which is what a key count means.
func keyLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}
