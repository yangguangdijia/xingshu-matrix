package backup

import (
	"reflect"
	"testing"
	"time"
)

func TestPruneCandidates(t *testing.T) {
	base := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.AddDate(0, 0, n) }
	list := []BackupInfo{
		{1, "scheduled", day(0)},
		{2, "manual", day(0)},
		{3, "scheduled", day(1)},
		{4, "pre_migrate", day(1)},
		{5, "scheduled", day(3)},
		{6, "scheduled", day(2)},
		{7, "manual", day(5)},
	}
	cases := []struct {
		name string
		list []BackupInfo
		keep int
		want []int64
	}{
		{"keep 2", list, 2, []int64{3, 1}},
		{"keep 1", list, 1, []int64{6, 3, 1}},
		{"keep all", list, 4, nil},
		{"keep more", list, 10, nil},
		{"keep 0 disables", list, 0, nil},
		{"keep negative disables", list, -1, nil},
		{"empty", nil, 3, nil},
		{"only manual", []BackupInfo{{1, "manual", day(0)}, {2, "pre_migrate", day(1)}}, 1, nil},
		{"tie broken by id", []BackupInfo{{10, "scheduled", day(0)}, {11, "scheduled", day(0)}}, 1, []int64{10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PruneCandidates(c.list, c.keep)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
