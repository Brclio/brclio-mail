//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package store

// Filesystem free-space inspection is unavailable on this platform. The
// conservative archive byte cap remains enforced.
func (s *Store) ensureDiskReserve(int64) error { return nil }

func saturatingMultiply(value, multiplier int64) int64 {
	if value <= 0 || multiplier <= 0 {
		return 0
	}
	if value > maxInt64/multiplier {
		return maxInt64
	}
	return value * multiplier
}
