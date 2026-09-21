package manifest_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// jobWith is one Package manifest whose container unit is a job carrying the
// block under test, so every case below differs by that block alone.
func jobWith(block string) string {
	return `name: weather
description: Fetch the forecast and post it to the group every morning.
deploy:
  units:
    - type: container
      build: .
      expose: none
      schedule: "0 8 * * *"
` + block
}

// A unit declares the expression and the surface reads it back as written,
// which is what pkg_inspect shows: the manifest is answered as it is, so a
// member reads the schedule where they wrote it.
func TestUnitCarriesTheDeclaredSchedule(t *testing.T) {
	m, err := manifest.Parse([]byte(jobWith("")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	units, err := m.Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) == 0 {
		t.Fatal("the manifest has no unit")
	}
	unit := units[0]
	if unit.Schedule != "0 8 * * *" {
		t.Errorf("the unit declares schedule %q, want %q", unit.Schedule, "0 8 * * *")
	}
	if !unit.Scheduled() {
		t.Error("the unit does not read as a job")
	}
	if !strings.Contains(fmt.Sprint(m.Raw), "0 8 * * *") {
		t.Error("the raw document does not carry the declared schedule, so pkg_inspect would not show it")
	}
	// A unit without one is a Process that stays up, which is every manifest
	// written before this existed.
	plain, err := manifest.Parse([]byte(`name: echo
description: Answer whatever is said to it, for testing the surface.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if plainUnits, _ := plain.Units(); plainUnits[0].Scheduled() {
		t.Error("a unit with no schedule reads as a job")
	}
}

// Every refusal a schedule can earn. A job is a container that runs and exits,
// so everything that describes a Process which stays up is refused beside it,
// and an expression kitbash cannot read is refused where it is written rather
// than at the tick that never comes.
func TestScheduleRefusals(t *testing.T) {
	cases := map[string]string{
		"an exposed unit":           strings.Replace(jobWith(""), "expose: none", "expose: http\n      port: 8080", 1),
		"a unit exposed as mcp":     strings.Replace(jobWith(""), "expose: none", "expose: mcp", 1),
		"a health probe":            jobWith("      health: { http: /healthz }\n"),
		"a restart policy":          jobWith("      restart: always\n"),
		"a restart policy of never": jobWith("      restart: never\n"),
		"a subscription": jobWith("") + `provides:
  subscriptions: [telemetry]
`,
		"four fields":      strings.Replace(jobWith(""), `"0 8 * * *"`, `"0 8 * *"`, 1),
		"a name for a day": strings.Replace(jobWith(""), `"0 8 * * *"`, `"0 8 * * mon"`, 1),
		"an hour of 24":    strings.Replace(jobWith(""), `"0 8 * * *"`, `"0 24 * * *"`, 1),
		"a macro":          strings.Replace(jobWith(""), `"0 8 * * *"`, `"@daily"`, 1),
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(document))
			if err == nil {
				t.Fatal("the manifest was accepted")
			}
			var invalid *manifest.ErrInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v, want an invalid manifest", err)
			}
			if len(invalid.Messages) == 0 {
				t.Error("the refusal names nothing")
			}
		})
	}
}

// A unit that declares no schedule keeps every one of those blocks: the rules
// are the job's, not the manifest's.
func TestTheScheduleRulesApplyToJobsAlone(t *testing.T) {
	document := `name: echo
description: Answer whatever is said to it, for testing the surface.
deploy:
  units:
    - type: container
      build: .
      expose: http
      port: 8080
      restart: always
      health: { http: /healthz, interval: 30s }
provides:
  subscriptions: [telemetry]
`
	if _, err := manifest.Parse([]byte(document)); err != nil {
		t.Fatalf("a Process that stays up was refused: %v", err)
	}
}
