// Package safepath answers one question the whole surface asks: is this
// relative path inside this folder, really.
//
// Four places used to answer it separately, and they disagreed: a build
// context, the files an import kit returns, a multi file write, and a schema
// reference in a manifest. A lexical check alone is not an answer, because a
// symlink component turns a path that looks inside into a path that is not, so
// every component is inspected on the way down.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Inside resolves rel against dir and returns the joined path, or says why the
// path is not one kitbash will touch. A path is inside dir when it is
// relative, has no empty, dot or parent component, and no component that
// exists today is a symlink. Components that do not exist yet are allowed:
// this answers where a file may be written as well as where one may be read.
//
// The single dot names dir itself, which is how a manifest spells a build
// context of the whole Package folder.
func Inside(dir, rel string) (string, error) {
	if rel == "." {
		return filepath.Clean(dir), nil
	}
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q is not a relative path inside this folder", rel)
	}
	current := filepath.Clean(dir)
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		switch {
		case segment == "" || segment == ".":
			return "", fmt.Errorf("%q has an empty path component", rel)
		case segment == "..":
			return "", fmt.Errorf("%q leaves this folder", rel)
		case strings.HasPrefix(segment, "."):
			return "", fmt.Errorf("the path component %q begins with a dot, which is reserved", segment)
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			// The rest of the path does not exist yet, so it holds no links.
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%q passes through a symlink, and kitbash does not follow symlinks", rel)
		}
	}
	return current, nil
}
