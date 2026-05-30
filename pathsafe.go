package md4agents

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// resolveAndCheck returns the symlink-resolved absolute path of candidate,
// guaranteed to be inside resolvedRoot. Returns an error if the candidate
// does not exist or escapes the root once symlinks are followed.
//
// Both the root and the candidate are EvalSymlinks'd. This closes the gap
// where filepath.Rel alone happily accepted a textually-inside-Root path
// that pointed at /etc/passwd via a symlink.
func resolveAndCheck(resolvedRoot, candidate string) (string, error) {
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if !pathInside(resolvedRoot, real) {
		return "", errors.New("path escapes root via symlink")
	}
	return real, nil
}

// resolveParentAndCheck is the same idea but for a path that may not exist
// yet (e.g. a sidecar file we're about to write). It accepts the candidate
// when:
//  1. The candidate is lexically inside resolvedRoot.
//  2. No existing component between resolvedRoot and the candidate is a
//     symlink that escapes resolvedRoot once resolved.
//
// We don't follow the candidate's own dir tree downward because if a
// subdir is a non-existent path component we'll create it ourselves via
// MkdirAll; the new dir can't be an escape symlink.
func resolveParentAndCheck(resolvedRoot, candidate string) (string, error) {
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if !pathInside(resolvedRoot, abs) {
		return "", errors.New("path escapes root (lexical)")
	}
	cur := filepath.Dir(abs)
	for {
		if cur == resolvedRoot || len(cur) <= len(resolvedRoot) {
			return abs, nil
		}
		info, err := os.Lstat(cur)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			if !pathInside(resolvedRoot, real) {
				return "", errors.New("path escapes root via existing symlink")
			}
		}
		cur = filepath.Dir(cur)
	}
}

// pathInside is the lexical "is B inside A" check, used after both ends
// have been canonicalized. Avoids the prefix-trap (/var/www-evil vs /var/www).
func pathInside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
