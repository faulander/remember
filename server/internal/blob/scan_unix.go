//go:build darwin || linux

package blob

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

func scanBlobFiles(ctx context.Context, root *securedRoot) (map[string]struct{}, int, int, error) {
	fd, err := rootFD(root)
	if err != nil {
		return nil, 0, 0, err
	}
	seen := make(map[string]struct{})
	malformed, symlinks, err := scanDirectoryAt(ctx, fd, 0, "", seen)
	return seen, malformed, symlinks, err
}

func scanDirectoryAt(ctx context.Context, fd, depth int, prefix string, seen map[string]struct{}) (int, int, error) {
	file := os.NewFile(uintptr(fd), "blob-audit-directory")
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return 0, 0, err
	}
	malformed, symlinks := 0, 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return malformed, symlinks, err
		}
		name := entry.Name()
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return malformed, symlinks, err
		}
		kind := stat.Mode & unix.S_IFMT
		if kind == unix.S_IFLNK {
			symlinks++
			continue
		}
		if kind == unix.S_IFDIR {
			valid := (depth == 0 && name == "sha256") ||
				(depth == 1 && validHex(name, 2)) ||
				(depth == 2 && validHex(name, 2))
			if !valid {
				malformed++
				continue
			}
			if stat.Mode&0o777 != 0o700 {
				malformed++
			}
			next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return malformed, symlinks, err
			}
			nextPrefix := prefix
			if depth == 1 || depth == 2 {
				nextPrefix += name
			}
			childMalformed, childSymlinks, err := scanDirectoryAt(ctx, next, depth+1, nextPrefix, seen)
			malformed += childMalformed
			symlinks += childSymlinks
			if err != nil {
				return malformed, symlinks, err
			}
			continue
		}
		if kind != unix.S_IFREG || depth != 3 || !validHex(name, 64) || len(prefix) != 4 || name[:4] != prefix {
			malformed++
			continue
		}
		if stat.Mode&0o777 != 0o600 {
			malformed++
		}
		seen[name] = struct{}{}
	}
	return malformed, symlinks, nil
}
