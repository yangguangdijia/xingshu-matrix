package backup

import (
	"sort"
	"time"
)

// BackupInfo is the minimal view of a backup needed for retention.
type BackupInfo struct {
	ID        int64
	Kind      string
	CreatedAt time.Time
}

// PruneCandidates returns IDs of "scheduled" backups beyond the newest keep.
// Other kinds (manual, pre_migrate, ...) are never pruned. keep <= 0 disables
// pruning. Ties on CreatedAt are broken by higher ID being newer.
func PruneCandidates(list []BackupInfo, keep int) []int64 {
	if keep <= 0 {
		return nil
	}
	var sched []BackupInfo
	for _, b := range list {
		if b.Kind == "scheduled" {
			sched = append(sched, b)
		}
	}
	if len(sched) <= keep {
		return nil
	}
	sort.SliceStable(sched, func(i, j int) bool {
		if !sched[i].CreatedAt.Equal(sched[j].CreatedAt) {
			return sched[i].CreatedAt.After(sched[j].CreatedAt)
		}
		return sched[i].ID > sched[j].ID
	})
	out := make([]int64, 0, len(sched)-keep)
	for _, b := range sched[keep:] {
		out = append(out, b.ID)
	}
	return out
}
