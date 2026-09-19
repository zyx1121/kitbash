package manifest_test

import (
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// at is one UTC instant, which is the only zone a schedule is read in.
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// Every kind of field the dialect accepts, read as the next tick after a
// moment. One case per shape: a star, a number, a list, a range, a step, a
// step over a range, and the two day fields together.
func TestCronReadsEveryFieldKind(t *testing.T) {
	cases := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{"every minute", "* * * * *", at(2026, 9, 19, 9, 30), at(2026, 9, 19, 9, 31)},
		{"a number", "0 8 * * *", at(2026, 9, 19, 9, 30), at(2026, 9, 20, 8, 0)},
		{"a number later today", "0 18 * * *", at(2026, 9, 19, 9, 30), at(2026, 9, 19, 18, 0)},
		{"a list", "0 8,12,18 * * *", at(2026, 9, 19, 9, 30), at(2026, 9, 19, 12, 0)},
		{"a range", "0 9-17 * * *", at(2026, 9, 19, 20, 0), at(2026, 9, 20, 9, 0)},
		{"a step", "*/15 * * * *", at(2026, 9, 19, 9, 1), at(2026, 9, 19, 9, 15)},
		{"a step over a range", "0 9-17/4 * * *", at(2026, 9, 19, 10, 0), at(2026, 9, 19, 13, 0)},
		{"a step from a number", "5/15 * * * *", at(2026, 9, 19, 9, 6), at(2026, 9, 19, 9, 20)},
		{"a day of the month", "0 0 1 * *", at(2026, 9, 19, 9, 0), at(2026, 10, 1, 0, 0)},
		{"a month", "0 0 1 1 *", at(2026, 9, 19, 9, 0), at(2027, 1, 1, 0, 0)},
		{"a weekday", "30 6 * * 1", at(2026, 9, 19, 9, 0), at(2026, 9, 21, 6, 30)},
		{"a step in the widest range", "0-59/60 * * * *", at(2026, 9, 19, 9, 30), at(2026, 9, 19, 10, 0)},
		{"sunday written seven", "30 6 * * 7", at(2026, 9, 19, 9, 0), at(2026, 9, 20, 6, 30)},
		{"sunday written zero", "30 6 * * 0", at(2026, 9, 19, 9, 0), at(2026, 9, 20, 6, 30)},
		{"weekdays", "0 9 * * 1-5", at(2026, 9, 19, 9, 0), at(2026, 9, 21, 9, 0)},
		// Both day fields restricted is the one rule of cron that is not a
		// lookup: the day matches if either does.
		{"either day field", "0 0 1 * 1", at(2026, 9, 19, 9, 0), at(2026, 9, 21, 0, 0)},
		{"either day field, the month day next", "0 9 1 * 1", at(2026, 9, 30, 0, 0), at(2026, 10, 1, 9, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cron, err := manifest.ParseCron(c.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", c.expr, err)
			}
			got, ok := cron.Next(c.from)
			if !ok {
				t.Fatalf("%q has no tick after %s", c.expr, c.from)
			}
			if !got.Equal(c.want) {
				t.Errorf("%q after %s = %s, want %s", c.expr, c.from, got, c.want)
			}
		})
	}
}

// A day field that begins with * is unrestricted for the either rule whatever
// step follows it, which is what every other cron does: "0 9 */2 * 1" is every
// other day and Mondays read together, so it fires on Mondays and nothing
// else. Reading the step as a restriction would fire it every second day too.
func TestAStepInADayFieldIsStillAStar(t *testing.T) {
	cron, err := manifest.ParseCron("0 9 */2 * 1")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	// Wednesday 2 September 2026 is a day the step matches and the weekday
	// does not.
	next, ok := cron.Next(at(2026, 9, 2, 0, 0))
	if !ok {
		t.Fatal("no tick")
	}
	if next.Weekday() != time.Monday {
		t.Errorf("the next tick is %s, a %s; a day field beginning with * is not a restriction",
			next, next.Weekday())
	}
	if !next.Equal(at(2026, 9, 7, 9, 0)) {
		t.Errorf("the next tick is %s, want Monday 7 September at 09:00", next)
	}
	// The other way round is a restriction: a day of the month written out
	// with a weekday is the either rule.
	both, err := manifest.ParseCron("0 9 2 * 1")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if next, _ := both.Next(at(2026, 9, 1, 0, 0)); !next.Equal(at(2026, 9, 2, 9, 0)) {
		t.Errorf("0 9 2 * 1 from 1 September = %s, want the 2nd", next)
	}
}

// A tick is strictly after the moment it is computed from, so a job that ran at
// 09:00 is not due at 09:00 again, and the seconds of the moment are not a
// second chance at the same minute.
func TestTheNextTickIsStrictlyAfterTheMomentGiven(t *testing.T) {
	cron, err := manifest.ParseCron("0 9 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	nine := at(2026, 9, 19, 9, 0)
	if got, _ := cron.Next(nine); !got.Equal(at(2026, 9, 20, 9, 0)) {
		t.Errorf("after 09:00 exactly = %s, want tomorrow", got)
	}
	if got, _ := cron.Next(nine.Add(-time.Second)); !got.Equal(nine) {
		t.Errorf("after 08:59:59 = %s, want 09:00 today", got)
	}
	// And a moment part way through a minute is not that minute again.
	if got, _ := cron.Next(nine.Add(30 * time.Second)); !got.Equal(at(2026, 9, 20, 9, 0)) {
		t.Errorf("after 09:00:30 = %s, want tomorrow", got)
	}
}

// The search crosses the end of a month, the end of a year, and a February
// that has a 29th only every fourth year.
func TestTheNextTickCrossesMonthAndYearBoundaries(t *testing.T) {
	cases := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{"into the next month", "0 0 1 * *", at(2026, 1, 31, 23, 59), at(2026, 2, 1, 0, 0)},
		{"into the next year", "59 23 31 12 *", at(2026, 12, 31, 23, 58), at(2026, 12, 31, 23, 59)},
		{"over the new year", "0 0 1 1 *", at(2026, 12, 31, 23, 59), at(2027, 1, 1, 0, 0)},
		{"the 29th of February", "0 0 29 2 *", at(2026, 3, 1, 0, 0), at(2028, 2, 29, 0, 0)},
		{"the 31st of a short month", "0 0 31 * *", at(2026, 4, 1, 0, 0), at(2026, 5, 31, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cron, err := manifest.ParseCron(c.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", c.expr, err)
			}
			got, ok := cron.Next(c.from)
			if !ok {
				t.Fatalf("%q has no tick after %s", c.expr, c.from)
			}
			if !got.Equal(c.want) {
				t.Errorf("%q after %s = %s, want %s", c.expr, c.from, got, c.want)
			}
		})
	}
}

// A date no calendar has matches nothing, which is answered rather than
// searched for ever.
func TestADateNoCalendarHasHasNoTick(t *testing.T) {
	cron, err := manifest.ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if got, ok := cron.Next(at(2026, 1, 1, 0, 0)); ok {
		t.Errorf("the 30th of February has a tick at %s", got)
	}
}

// The reading is in UTC whatever the moment it is given carries, so a schedule
// means the same thing on every host and no hour happens twice.
func TestTheNextTickIsReadInUTC(t *testing.T) {
	cron, err := manifest.ParseCron("0 8 * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	taipei := time.FixedZone("Asia/Taipei", 8*60*60)
	// 16:30 in Taipei is 08:30 UTC, so the next tick is tomorrow at 08:00 UTC.
	got, ok := cron.Next(time.Date(2026, 9, 19, 16, 30, 0, 0, taipei))
	if !ok {
		t.Fatal("no tick")
	}
	if !got.Equal(at(2026, 9, 20, 8, 0)) {
		t.Errorf("next tick = %s, want 2026-09-20T08:00:00Z", got)
	}
	if got.Location() != time.UTC {
		t.Errorf("the tick is in %s, want UTC", got.Location())
	}
}

// The shapes the dialect refuses. Each is a manifest that would otherwise sit
// on a host as a job that never runs, or runs at a time nobody wrote.
func TestCronRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"four fields", "0 8 * *"},
		{"six fields", "0 0 8 * * *"},
		{"a name for a day", "0 8 * * mon"},
		{"a name for a month", "0 8 * jan *"},
		{"a macro", "@daily"},
		{"a minute of 60", "60 8 * * *"},
		{"an hour of 24", "0 24 * * *"},
		{"a day of the month of 0", "0 8 0 * *"},
		{"a month of 13", "0 8 * 13 *"},
		{"a day of the week of 8", "0 8 * * 8"},
		{"a backwards range", "0 17-9 * * *"},
		{"a step of zero", "*/0 * * * *"},
		{"a step that is not a number", "*/x * * * *"},
		{"an empty list entry", "0,,30 * * * *"},
		{"a question mark", "0 8 ? * *"},
		{"the last day", "0 8 L * *"},
		{"a negative minute", "-5 8 * * *"},
		// The two the schema's own pattern refuses, which this has to refuse
		// as well or a manifest is read two ways.
		{"a signed minute", "+5 8 * * *"},
		{"a line break between fields", "0 8 * *\n*"},
		{"a line break inside a field", "0 8 * * 1\n"},
		{"unicode digits", "\u0665 8 * * *"},
		{"a minute of three digits", "100 * * * *"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := manifest.ParseCron(c.expr); err == nil {
				t.Errorf("ParseCron(%q) was accepted", c.expr)
			}
			if manifest.ValidCron(c.expr) {
				t.Errorf("ValidCron(%q) is true", c.expr)
			}
		})
	}
}

// Whitespace between fields is whatever the member wrote, and the expression a
// parsed schedule reports is the one it read.
func TestCronReadsAnyWhitespaceBetweenFields(t *testing.T) {
	cron, err := manifest.ParseCron("0   8 *\t* *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if cron.Expr != "0 8 * * *" {
		t.Errorf("Expr = %q, want the five fields joined by single spaces", cron.Expr)
	}
}
