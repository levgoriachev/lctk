package httpapi

import (
	"testing"
	"time"
)

func TestSemanticStallRequiresThreeMinutesWithoutProgress(t *testing.T) {
	now := time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC)
	if stalled, seconds := semanticStall(now.Add(-179*time.Second).Format(time.RFC3339Nano), now); stalled || seconds != 179 {
		t.Fatalf("179 second status = stalled:%t seconds:%d", stalled, seconds)
	}
	if stalled, seconds := semanticStall(now.Add(-181*time.Second).Format(time.RFC3339Nano), now); !stalled || seconds != 181 {
		t.Fatalf("181 second status = stalled:%t seconds:%d", stalled, seconds)
	}
}

func TestSemanticStallRejectsInvalidOrFutureProgressTime(t *testing.T) {
	now := time.Now().UTC()
	for _, value := range []string{"not-a-time", now.Add(time.Minute).Format(time.RFC3339Nano)} {
		if stalled, seconds := semanticStall(value, now); stalled || seconds != 0 {
			t.Fatalf("progress %q = stalled:%t seconds:%d", value, stalled, seconds)
		}
	}
}
