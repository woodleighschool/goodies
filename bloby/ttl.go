package bloby

import "time"

func ttlOrDefault(ttl time.Duration, fallback time.Duration) time.Duration {
	if ttl > 0 {
		return ttl
	}
	return fallback
}
