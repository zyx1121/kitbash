package manifest

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A unit with expose: none may declare schedule, five field cron syntax read
// in UTC, and kitbashd starts it at each tick, see PLAN.md section 2.3. The
// dialect is spelled here because it is manifest vocabulary: the member writes
// the expression, spec/manifest.schema.json holds it to a shape, and this is
// the one thing that decides what it means and when it fires. Two readings of
// one expression would be two schedules.

// CronFields is how many fields an expression carries: minute, hour, day of
// month, month, day of week. Seconds are not a field and never will be: a host
// that starts containers is not a thing to ask for every second.
const CronFields = 5

// MaxCronBytes bounds one expression. Five fields of lists are a line, and an
// expression longer than this is not one a person wrote.
const MaxCronBytes = 256

// cronSearchYears bounds the search for the next tick. An expression that
// names a day no month has, 31 February written as dom 31 and month 2, matches
// nothing: the search gives up after this rather than walking the calendar for
// ever.
const cronSearchYears = 5

// Cron is one parsed five field expression: a bitmap per field, and whether
// the two day fields were restricted, which is what decides how they combine.
//
// Expr is the expression as the manifest wrote it, kept so a caller that has
// to say which schedule it is talking about says the member's own words.
type Cron struct {
	Expr string

	minute uint64 // 0-59
	hour   uint64 // 0-23
	dom    uint64 // 1-31
	month  uint64 // 1-12
	dow    uint64 // 0-6, Sunday is 0

	// domAny and dowAny are the day fields that begin with *. When both day
	// fields are restricted a day matches if either one does, which is what
	// cron has always done and what "0 9 1 * 1" means: the first of the month
	// and every Monday, not the Mondays that fall on the first.
	//
	// A field that begins with * is unrestricted for that rule whatever step
	// follows it, which is what Vixie cron's DOM_STAR is: "0 9 */2 * 1" is
	// every other day and Mondays read together, so it fires on Mondays and
	// nothing else. Reading a step as a restriction would make it fire on
	// every second day as well, which is not what any other cron does.
	domAny bool
	dowAny bool
}

// cronField is one field's name and the range it accepts.
type cronField struct {
	name string
	min  int
	max  int
}

var cronFields = [CronFields]cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7},
}

// ParseCron reads one five field expression. The accepted dialect is *, a
// number, a range a-b, a step */s or a-b/s, and a comma separated list of any
// of those. Names are not accepted: mon and jan are two spellings of what a
// number already says, and a manifest is read by more than one program.
//
// Day of week takes 0 to 7 with both 0 and 7 meaning Sunday, which is what
// every cron accepts and what a member who writes 7 means.
func ParseCron(expr string) (Cron, error) {
	if len(expr) > MaxCronBytes {
		return Cron{}, fmt.Errorf("manifest: the schedule is %d bytes, over the %d a cron expression may be",
			len(expr), MaxCronBytes)
	}
	parts := fields(expr)
	if len(parts) != CronFields {
		return Cron{}, fmt.Errorf("manifest: %q has %d fields, not the %d of minute hour day-of-month month day-of-week",
			expr, len(parts), CronFields)
	}
	c := Cron{Expr: strings.Join(parts, " ")}
	var bits [CronFields]uint64
	for i, part := range parts {
		set, any, err := parseCronField(part, cronFields[i])
		if err != nil {
			return Cron{}, err
		}
		bits[i] = set
		switch i {
		case 2:
			c.domAny = any
		case 4:
			c.dowAny = any
		}
	}
	c.minute, c.hour, c.dom, c.month, c.dow = bits[0], bits[1], bits[2], bits[3], bits[4]
	// Sunday is written 0 and 7 and is one day, so the two spellings are one
	// bit before anything compares a weekday to this.
	if c.dow&(1<<7) != 0 {
		c.dow |= 1 << 0
		c.dow &^= 1 << 7
	}
	return c, nil
}

// fields splits an expression on the two characters that separate its fields.
// It is not strings.Fields: a line break is not a separator here, because
// spec/manifest.schema.json holds the whole expression to one line, and an
// expression this parser read and the schema refused would be two readings of
// one manifest.
func fields(expr string) []string {
	return strings.FieldsFunc(expr, func(r rune) bool { return r == ' ' || r == '\t' })
}

// number reads one field's number. It takes digits and nothing else: strconv
// takes a sign, and "+5" is a minute the schema refuses and this would
// otherwise accept, which is the same two readings.
func number(text string) (int, bool) {
	if text == "" || len(text) > 2 {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ValidCron reports whether an expression is one kitbashd runs, which is what
// a caller that only has to decide asks.
func ValidCron(expr string) bool {
	_, err := ParseCron(expr)
	return err == nil
}

// parseCronField reads one field into a bitmap and reports whether it was
// written as *, which the day fields are read differently for.
func parseCronField(field string, f cronField) (uint64, bool, error) {
	refuse := func(reason string) (uint64, bool, error) {
		return 0, false, fmt.Errorf("manifest: %q is not a %s field of a cron expression: %s",
			field, f.name, reason)
	}
	if field == "" {
		return refuse("it is empty")
	}
	var set uint64
	any := false
	for _, entry := range strings.Split(field, ",") {
		if entry == "" {
			return refuse("it carries an empty list entry")
		}
		spec, step := entry, 1
		if base, written, found := strings.Cut(entry, "/"); found {
			n, ok := number(written)
			if !ok || n < 1 {
				return refuse("the step after / is not a number of at least 1")
			}
			if n > f.max-f.min+1 {
				return refuse(fmt.Sprintf("the step %d is wider than the field", n))
			}
			spec, step = base, n
		}
		low, high := f.min, f.max
		switch {
		case spec == "*":
			// A field that begins with * is unrestricted for the either rule
			// the two day fields are read by, whatever step follows it, see
			// Cron.domAny.
			any = true
		default:
			from, to, ranged := strings.Cut(spec, "-")
			n, ok := number(from)
			if !ok {
				return refuse(fmt.Sprintf("%q is not a number", from))
			}
			if n < f.min || n > f.max {
				return refuse(fmt.Sprintf("%d is outside %d to %d", n, f.min, f.max))
			}
			low, high = n, n
			if ranged {
				m, ok := number(to)
				if !ok {
					return refuse(fmt.Sprintf("%q is not a number", to))
				}
				if m < f.min || m > f.max {
					return refuse(fmt.Sprintf("%d is outside %d to %d", m, f.min, f.max))
				}
				if m < n {
					return refuse(fmt.Sprintf("the range %d-%d ends before it begins", n, m))
				}
				high = m
			} else if step > 1 {
				// A step on a single number is the range from it to the end
				// of the field, which is what 5/15 means everywhere cron is
				// read.
				high = f.max
			}
		}
		for v := low; v <= high; v += step {
			set |= 1 << uint(v)
		}
	}
	if set == 0 {
		return refuse("it matches nothing")
	}
	return set, any, nil
}

// Next is the first tick strictly after the time given, in UTC. It answers
// false for an expression that matches no moment in the next few years, which
// is a date no calendar has, such as the 31st of February.
//
// The whole search is in UTC, so there is no hour that happens twice and none
// that does not happen: a schedule means the same thing on every host, which
// is why PLAN.md section 2.3 reads it in UTC and not in a member's zone.
func (c Cron) Next(after time.Time) (time.Time, bool) {
	// The tick is a minute, so the search starts at the beginning of the next
	// one: a run at 09:00:30 is not a second chance at 09:00.
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(cronSearchYears, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			// Every day of this month is wrong, so the search moves to the
			// first minute of the next one rather than walking it.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !c.matchesDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if c.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// matchesDay is the one rule of cron that is not a bitmap lookup: when both
// day fields are restricted the day matches if either does, and when one is *
// the other decides alone.
func (c Cron) matchesDay(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	default:
		return dom || dow
	}
}
