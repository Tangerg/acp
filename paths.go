package acp

import (
	"fmt"
	"strings"
)

// The specification says every path the protocol carries is absolute, and puts
// the obligation on whoever writes it, so this package refuses to send one that
// is not. The check accepts what either POSIX or Windows calls absolute, because
// the path describes the peer's filesystem rather than this process's: what the
// rule refuses is a path whose meaning depends on a working directory the two
// peers do not share.
func absolutePath(field, path string) error {
	if isAbsolutePath(path) {
		return nil
	}
	return fmt.Errorf("acp: %s must be an absolute path, and %q is not", field, path)
}

func isAbsolutePath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/"), strings.HasPrefix(path, `\\`):
		// A POSIX path, or a UNC share.
		return true
	case len(path) >= 3 && path[1] == ':' && (path[2] == '/' || path[2] == '\\'):
		// A Windows drive letter.
		letter := path[0] | ' '
		return letter >= 'a' && letter <= 'z'
	default:
		return false
	}
}

func absolutePaths(field string, paths []string) error {
	for i, path := range paths {
		if err := absolutePath(fmt.Sprintf("%s[%d]", field, i), path); err != nil {
			return err
		}
	}
	return nil
}

func absoluteOptionalPath(field string, path Opt[string]) error {
	if value, set := path.Get(); set {
		return absolutePath(field, value)
	}
	return nil
}
