//go:build windows

package repository

// Go cannot portably fsync a directory handle on Windows. os.Rename provides
// the atomic replacement boundary; file contents were flushed beforehand.
func syncDirectory(string) error { return nil }
