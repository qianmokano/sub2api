//go:build unit

package kanoupdate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNextBeijingScheduleIgnoresHostTimezoneAndDST(t *testing.T) {
	for _, test := range []struct{ now, want string }{
		{"2026-10-16T19:59:59Z", "2026-10-16T20:00:00Z"},
		{"2026-10-16T20:00:00Z", "2026-10-17T20:00:00Z"},
		{"2026-10-16T20:00:59Z", "2026-10-17T20:00:00Z"},
		{"2026-12-31T21:00:00Z", "2027-01-01T20:00:00Z"},
		{"2028-02-28T21:00:00Z", "2028-02-29T20:00:00Z"},
		{"2026-03-08T10:00:00Z", "2026-03-08T20:00:00Z"},
	} {
		for _, zone := range []string{"UTC", "America/Los_Angeles", "Europe/Berlin", "Asia/Tokyo"} {
			t.Run(test.now+zone, func(t *testing.T) {
				now, err := time.Parse(time.RFC3339, test.now)
				require.NoError(t, err)
				location, err := time.LoadLocation(zone)
				require.NoError(t, err)
				next := NextSchedule(now.In(location))
				require.Equal(t, test.want, next.Format(time.RFC3339))
				require.Equal(t, time.UTC, next.Location())
			})
		}
	}
}

func TestScheduleWindowAndSession(t *testing.T) {
	at := NextSchedule(time.Now())
	job := &Job{ScheduledAt: &at}
	require.False(t, job.InScheduleWindow(at.Add(-time.Nanosecond)))
	require.True(t, job.InScheduleWindow(at))
	require.True(t, job.InScheduleWindow(at.Add(time.Minute-time.Nanosecond)))
	require.False(t, job.InScheduleWindow(at.Add(time.Minute)))
	require.Equal(t, at.Add(24*time.Hour), job.SessionExpiresAt(at.Add(-time.Hour)))
	require.Equal(t, at.Add(25*time.Hour), job.SessionExpiresAt(at.Add(time.Hour)))
	job.ScheduledAt = nil
	require.True(t, job.InScheduleWindow(at))
	for _, status := range []Status{Scheduled, Cancelled, Missed} {
		require.True(t, status.Valid())
		require.False(t, status.Active())
		require.Equal(t, status == Scheduled, status.Reserved())
	}
	require.True(t, Queued.Reserved())
}
