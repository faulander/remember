//go:build windows

package blob

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func scanBlobFiles(ctx context.Context, root *securedRoot) (map[string]struct{}, int, int, error) {
	seen := make(map[string]struct{})
	malformed, symlinks := 0, 0
	err := filepath.WalkDir(root.path, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == root.path {
			return nil
		}
		relative, err := filepath.Rel(root.path, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			symlinks++
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			valid := (len(parts) == 1 && parts[0] == "sha256") ||
				(len(parts) == 2 && parts[0] == "sha256" && validHex(parts[1], 2)) ||
				(len(parts) == 3 && parts[0] == "sha256" && validHex(parts[1], 2) && validHex(parts[2], 2))
			if !valid {
				malformed++
				return filepath.SkipDir
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
				malformed++
			}
			return nil
		}
		if !info.Mode().IsRegular() || len(parts) != 4 || parts[0] != "sha256" ||
			!validHex(parts[1], 2) || !validHex(parts[2], 2) || !validHex(parts[3], 64) ||
			parts[1] != parts[3][:2] || parts[2] != parts[3][2:4] {
			malformed++
			return nil
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			malformed++
		}
		seen[parts[3]] = struct{}{}
		return nil
	})
	return seen, malformed, symlinks, err
}
