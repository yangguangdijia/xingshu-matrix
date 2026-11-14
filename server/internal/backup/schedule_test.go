package backup

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestScheduleNext(t *testing.T) {
	cases := []struct {
		name, expr, after, want string
	}{
		{"daily 2am same day", "0 2 * * *", "2026-11-14T01:30:00Z", "2026-11-14T02:00:00Z"},
		{"daily 2am exact is exclusive", "0 2 * * *", "2026-11-14T02:00:00Z", "2026-11-15T02:00:00Z"},
		{"every 15m", "*/15 * * * *", "2026-11-14T10:07:30Z", "2026-11-14T10:15:00Z"},
		{"every 15m hour wrap", "*/15 * * * *", "2026-11-14T10:45:00Z", "2026-11-14T11:00:00Z"},
		{"weekdays from Saturday", "0 3 * * 1-5", "2026-11-14T04:00:00Z", "2026-11-16T03:00:00Z"},
		{"weekdays from Monday early", "0 3 * * 1-5", "2026-11-16T02:59:00Z", "2026-11-16T03:00:00Z"},
		{"cross month", "0 2 * * *", "2026-11-30T03:00:00Z", "2026-12-01T02:00:00Z"},
		{"cross year", "0 0 1 * *", "2026-12-15T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"day 31 skips short months", "0 0 31 * *", "2026-11-01T00:00:00Z", "2026-12-31T00:00:00Z"},
		{"leap day", "0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"dow 7 is sunday", "0 0 * * 7", "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"dow 0 is sunday", "0 0 * * 0", "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"},
		// 2026-11-14 is Saturday: dom=20 or Monday -> Monday 16th comes first.
		{"dom/dow union", "0 0 20 * 1", "2026-11-14T00:00:00Z", "2026-11-16T00:00:00Z"},
		{"dom/dow union dom first", "0 0 15 * 3", "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"list and range", "5,10-12 8 * * *", "2026-11-14T08:10:00Z", "2026-11-14T08:11:00Z"},
		{"range step", "0 1-23/6 * * *", "2026-11-14T08:00:00Z", "2026-11-14T13:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := ParseCron(c.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", c.expr, err)
			}
			got := s.Next(mustTime(t, c.after))
			if want := mustTime(t, c.want); !got.Equal(want) {
				t.Fatalf("Next(%s) = %s, want %s", c.after, got.Format(time.RFC3339), c.want)
			}
		})
	}
}

func TestScheduleNextLocation(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	s, _ := ParseCron("0 2 * * *")
	got := s.Next(time.Date(2026, 11, 14, 18, 0, 0, 0, loc))
	if want := time.Date(2026, 11, 15, 2, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestScheduleNextNever(t *testing.T) {
	s, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(mustTime(t, "2026-01-01T00:00:00Z")); !got.IsZero() {
		t.Fatalf("expected zero time, got %v", got)
	}
}

func TestParseCronInvalid(t *testing.T) {
	bad := []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * 32 * *", "* * * 13 * ", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *",
		"*/x * * * *", "1-2-3 * * * *", "-1 * * * *",
	}
	for _, e := range bad {
		if _, err := ParseCron(e); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want error", e)
		}
	}
}

func TestSlotKey(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 11, 14, 2, 0, 59, 999, time.UTC), "2026-11-14T02:00Z"},
		{time.Date(2026, 11, 14, 10, 0, 0, 0, time.FixedZone("CST", 8*3600)), "2026-11-14T02:00Z"},
		{time.Date(2026, 11, 14, 1, 30, 0, 0, time.FixedZone("X", 9*3600)), "2026-11-13T16:30Z"},
	}
	for _, c := range cases {
		if got := SlotKey(c.in); got != c.want {
			t.Errorf("SlotKey(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
