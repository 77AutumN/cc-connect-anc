package core

import "fmt"

// ParseImageCacheCapacityMiB validates an explicit image cache capacity.
// Deployment-specific per-runtime and aggregate limits are checked by the host.
func ParseImageCacheCapacityMiB(value any) (int64, error) {
	var mib int64
	switch n := value.(type) {
	case int:
		mib = int64(n)
	case int64:
		mib = n
	default:
		return 0, fmt.Errorf("image_cache_capacity_mib must be an integer")
	}
	if mib < 1 || mib > 512 {
		return 0, fmt.Errorf("image_cache_capacity_mib must be between 1 and 512")
	}
	return mib, nil
}
