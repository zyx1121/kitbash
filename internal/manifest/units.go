package manifest

import (
	"fmt"
	"regexp"
	"sort"
)

// unitName is the shape of deploy.units[].name, which is also what
// spec/manifest.schema.json holds the field to: a short lower case word a
// container name, a log line and a Telemetry attribute can all carry.
var unitName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidUnitName reports whether a name is one a unit may be called. It is
// spelled here for the same reason ValidSecretName is: the rule belongs to the
// manifest, and two spellings of it would be two rules.
func ValidUnitName(name string) bool { return unitName.MatchString(name) }

// checkUnits is the part of the unit rules no JSON Schema can express, which
// is every rule that is about the units together rather than about one of
// them: a Package with more than one unit names each of them, no two of them
// share a name, and exactly one of them declares the exposure that is the
// Process's face, see PLAN.md section 5.6.
//
// A face is mcp or http. `expose: none` is what a unit behind the face
// declares, which is the same thing as declaring nothing: a sidecar that
// writes it out loud is not a second face, and a Package where every unit is
// none has nobody to answer for it.
//
// A Package with one unit is left as it was. Its name is optional, because the
// Package is what that unit is called, and its exposure defaults to none the
// way the schema says, because that is how every manifest written before this
// reads. The face rule therefore has something to decide only where there is
// more than one unit to decide between. The shape of a name is read whatever
// the count, because it is the one rule here that is about one unit: the
// schema holds a parsed manifest to it, and this holds a Manifest a caller
// built by hand to the same.
func checkUnits(raw map[string]any) []string {
	list := units(raw)
	var messages []string
	seen := map[string]int{}
	var faces []string
	for i, unit := range list {
		where := fmt.Sprintf("/deploy/units/%d", i)
		name, _ := unit["name"].(string)
		if name != "" && !ValidUnitName(name) {
			messages = append(messages, fmt.Sprintf(
				"%s/name: %q is not a unit name, which is lower case letters, digits and hyphens, at most 32 of them", where, name))
			continue
		}
		if len(list) < 2 {
			continue
		}
		switch {
		case name == "":
			messages = append(messages, fmt.Sprintf(
				"%s: a Package with more than one unit names every unit, and this one declares no name", where))
		case seen[name] > 0:
			messages = append(messages, fmt.Sprintf(
				"%s: the name %s is already the name of deploy.units[%d]", where, name, seen[name]-1))
		default:
			seen[name] = i + 1
		}
		if expose, _ := unit["expose"].(string); expose == ExposeMCP || expose == ExposeHTTP {
			faces = append(faces, unitLabel(unit, i))
		}
	}
	if len(list) < 2 {
		sort.Strings(messages)
		return messages
	}
	if len(faces) == 0 {
		messages = append(messages,
			"/deploy/units: exactly one unit declares expose as mcp or http, which is the face of the Process, and none of these units does")
	}
	if len(faces) > 1 {
		messages = append(messages, fmt.Sprintf(
			"/deploy/units: exactly one unit declares expose as mcp or http, which is the face of the Process, and %s do",
			joinNames(faces)))
	}
	sort.Strings(messages)
	return messages
}

// unitLabel is how a message names one unit: by the name it declared, or by
// its index when it declared none, so a manifest that is refused for having no
// names is still readable.
func unitLabel(unit map[string]any, i int) string {
	if name, _ := unit["name"].(string); name != "" {
		return name
	}
	return fmt.Sprintf("deploy.units[%d]", i)
}

// joinNames lists the units a message is about, in English.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	out := ""
	for i, name := range names[:len(names)-1] {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out + " and " + names[len(names)-1]
}
