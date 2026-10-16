package kanoupdate

import (
	"time"
	_ "time/tzdata" // The host binary must work without installed timezone files.
)

const ScheduleCapability = "schedule-beijing-0400"

// NextSchedule always chooses the next 04:00 in Beijing, even at exactly 04:00.
func NextSchedule(now time.Time) time.Time {
	zone, _ := time.LoadLocation("Asia/Shanghai")
	local := now.In(zone)
	next := time.Date(local.Year(), local.Month(), local.Day(), 4, 0, 0, 0, zone)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UTC()
}

func (j *Job) SessionExpiresAt(now time.Time) time.Time {
	if j.ScheduledAt != nil && j.ScheduledAt.After(now) {
		return j.ScheduledAt.Add(24 * time.Hour)
	}
	return now.Add(24 * time.Hour)
}

func (j *Job) InScheduleWindow(now time.Time) bool {
	return j.ScheduledAt == nil || (!now.Before(*j.ScheduledAt) && now.Before(j.ScheduledAt.Add(time.Minute)))
}
