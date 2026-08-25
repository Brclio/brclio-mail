//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package store

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (s *Store) ensureDiskReserve(incomingBytes int64) error {
	if s.minFreeDisk <= 0 || s.databasePath == "" || s.databasePath == ":memory:" {
		return nil
	}
	var info unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(s.databasePath), &info); err != nil {
		return fmt.Errorf("inspect SQLite filesystem capacity: %w", err)
	}
	blockSize := int64(info.Bsize)
	availableBlocks := int64(info.Bavail)
	available := maxInt64
	if blockSize > 0 && availableBlocks >= 0 && availableBlocks <= maxInt64/blockSize {
		available = availableBlocks * blockSize
	}
	// SQLite WAL and checkpointing can temporarily need another copy of the
	// incoming pages. Keep that headroom in addition to the configured reserve.
	writeHeadroom := saturatingMultiply(incomingBytes, 2)
	if available < s.minFreeDisk || available-s.minFreeDisk < writeHeadroom {
		return ErrArchiveFull
	}
	return nil
}

func saturatingMultiply(value, multiplier int64) int64 {
	if value <= 0 || multiplier <= 0 {
		return 0
	}
	if value > maxInt64/multiplier {
		return maxInt64
	}
	return value * multiplier
}
