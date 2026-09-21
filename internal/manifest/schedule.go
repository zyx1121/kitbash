package manifest

import (
	"fmt"
	"sort"
)

// The rules a scheduled unit is held to, which no JSON Schema can express. A
// unit that declares schedule is a job: kitbashd starts its container at each
// tick and the run is over when the entrypoint exits, see PLAN.md section 2.3.
// Everything that describes a Process which stays up is therefore refused
// beside it rather than ignored, because a manifest that declares both is a
// manifest whose author expected one of the two to happen.

// checkSchedule reads every unit that declares a schedule and answers what is
// wrong with it. The expression itself is parsed here as well: an expression
// kitbashd cannot read is refused where the manifest is written, not at the
// first tick that never comes.
//
// The subscription rule reaches outside the unit because the declaration does:
// provides.subscriptions is the Package's, and a subscriber is a Process that
// is up to receive records. A job is not up, so a Package that declares both
// has asked for a fan out to a container that exists for a minute a day.
func checkSchedule(raw map[string]any) []string {
	var messages []string
	subscribes := len(subscriptionsOf(raw)) > 0
	for i, unit := range units(raw) {
		expr, ok := unit["schedule"].(string)
		if !ok || expr == "" {
			continue
		}
		where := fmt.Sprintf("/deploy/units/%d/schedule", i)
		if _, err := ParseCron(expr); err != nil {
			messages = append(messages, fmt.Sprintf("%s: %s", where, err))
		}
		if expose, held := unit["expose"].(string); held && expose != ExposeNone {
			messages = append(messages, fmt.Sprintf(
				"%s: a scheduled unit is a job, and this one declares expose: %s, which is a Process that stays up",
				where, expose))
		}
		if _, held := unit["health"]; held {
			messages = append(messages, fmt.Sprintf(
				"%s: a scheduled unit declares no health probe, because between its runs there is nothing to probe",
				where))
		}
		if _, held := unit["restart"]; held {
			messages = append(messages, fmt.Sprintf(
				"%s: a scheduled unit declares no restart policy, because the schedule is what starts it again",
				where))
		}
		if subscribes {
			messages = append(messages, fmt.Sprintf(
				"%s: this Package declares provides.subscriptions, which is a Process that is up to receive records, and a scheduled unit is not",
				where))
		}
	}
	sort.Strings(messages)
	return messages
}

// subscriptionsOf is provides.subscriptions as the document carries it, for a
// check that runs before the manifest is a Manifest.
func subscriptionsOf(raw map[string]any) []any {
	provides, ok := raw["provides"].(map[string]any)
	if !ok {
		return nil
	}
	list, _ := provides["subscriptions"].([]any)
	return list
}

// ScheduleFix is what an author does about a unit kitbash refuses to schedule.
// It is one sentence because the messages above say which rule was broken.
const ScheduleFix = "Declare deploy.units[].schedule as five cron fields read in UTC, such as \"0 8 * * *\", " +
	"on a unit with expose: none and no health, restart or subscriptions."
