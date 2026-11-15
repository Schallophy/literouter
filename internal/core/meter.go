package core

import (
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

// IsThrottled reports whether the model is inside its throttle cooldown.
func IsThrottled(record *model.ModelRecord, nowMs int64) bool {
	if record == nil || record.ThrottledUntil == nil {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, *record.ThrottledUntil)
	if err != nil {
		return false
	}
	return until.UnixMilli() > nowMs
}
