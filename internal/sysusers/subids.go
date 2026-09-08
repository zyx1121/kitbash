package sysusers

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The subordinate id layout rootless podman needs, see PLAN.md section 4.2.
// Every member gets one block of 65536 ids, the first at SubIDBase, and blocks
// never overlap: two members sharing a subuid range share container file
// ownership, which is not isolation at all.
const (
	SubIDBase  = 100000
	SubIDCount = 65536
	SubIDMode  = 0o644
)

// nextSubID reads a subordinate id file and answers the first free block above
// the highest one it holds. A file that does not exist yet answers SubIDBase,
// which is what a fresh host has.
func nextSubID(path string) (int, error) {
	lines, err := readLines(path)
	if err != nil {
		return 0, err
	}
	next := SubIDBase
	for _, line := range lines {
		_, start, count, ok := parseSubID(line)
		if !ok {
			continue
		}
		if end := start + count; end > next {
			next = end
		}
	}
	return next, nil
}

// hasSubID reports whether the file already carries a block for this name.
func hasSubID(path, name string) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		if owner, _, _, ok := parseSubID(line); ok && owner == name {
			return true, nil
		}
	}
	return false, nil
}

// appendSubID adds one block to a subordinate id file. The file is rewritten
// through a temporary file in the same directory and renamed over the
// original, so a reader never sees half a file and a crash never leaves
// /etc/subuid truncated.
func appendSubID(path, name string, start, count int) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	lines = append(lines, fmt.Sprintf("%s:%d:%d", name, start, count))
	return writeLines(path, lines)
}

// removeSubID drops every block belonging to one name and reports whether it
// removed any. Leaving the block behind would hand the next member with the
// same name the same ids.
func removeSubID(path, name string) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if owner, _, _, ok := parseSubID(line); ok && owner == name {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	return true, writeLines(path, kept)
}

// parseSubID reads one name:start:count line.
func parseSubID(line string) (name string, start, count int, ok bool) {
	fields := strings.Split(strings.TrimSpace(line), ":")
	if len(fields) != 3 {
		return "", 0, 0, false
	}
	start, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, 0, false
	}
	count, err = strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, 0, false
	}
	return fields[0], start, count, true
}

// readLines reads a subordinate id file, answering nothing for a file that
// does not exist. Blank lines are dropped so a rewrite does not grow them.
func readLines(path string) ([]string, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sysusers: read %s: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, "\r"))
		}
	}
	return out, nil
}

// writeLines replaces a subordinate id file atomically, keeping the mode
// shadow-utils expects.
func writeLines(path string, lines []string) error {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kitbash-subid-")
	if err != nil {
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := tmp.Chmod(SubIDMode); err != nil {
		tmp.Close()
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	return nil
}
