package manifest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// twoUnits is a Package of two container units, the second one the face, with
// the blocks each unit carries written by the caller so a case differs by the
// part under test alone.
func twoUnits(first, second string) string {
	return `name: board
description: A counter and the cache it keeps its count in, two units of one Package.
deploy:
  units:
    - type: container
      build: .
` + first + `    - type: container
      image: docker.io/library/redis@sha256:` + strings.Repeat("a", 64) + `
` + second
}

// The manifest layer reads every unit a Package declares, in the order the
// manifest wrote them, which is what stops version 1's units[0] from being the
// whole of what a Package can say, see PLAN.md section 5.6.
func TestUnitsReadsEveryUnit(t *testing.T) {
	m, err := manifest.Parse([]byte(twoUnits(
		"      name: web\n      expose: http\n      port: 8080\n",
		"      name: cache\n      command: [redis-server, --save, \"60 1\"]\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	units, err := m.Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("the manifest carries %d units, want both of them", len(units))
	}
	if units[0].Name != "web" || units[1].Name != "cache" {
		t.Errorf("the units are %q and %q, want them in manifest order", units[0].Name, units[1].Name)
	}
	if units[0].Expose != manifest.ExposeHTTP || units[1].Expose != manifest.ExposeNone {
		t.Errorf("the exposures are %q and %q, want the face and a unit behind it", units[0].Expose, units[1].Expose)
	}
	if len(units[1].Command) != 3 || units[1].Command[0] != "redis-server" || units[1].Command[2] != "60 1" {
		t.Errorf("the command is %v, want the words the unit declared", units[1].Command)
	}
}

// A unit behind the face may write expose: none out loud, which is the same
// as omitting the key: the face is the unit that declares mcp or http.
func TestASidecarMayDeclareExposeNone(t *testing.T) {
	m, err := manifest.Parse([]byte(twoUnits(
		"      name: web\n      expose: http\n      port: 8080\n",
		"      name: cache\n      expose: none\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	units, err := m.Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if units[1].Expose != manifest.ExposeNone {
		t.Errorf("the sidecar is exposed as %q, want none", units[1].Expose)
	}
}

// The rules a Package of several units is held to, each one its own guard.
func TestUnitsRefusals(t *testing.T) {
	cases := map[string]struct {
		first, second string
		says          string
	}{
		"no name on either unit": {
			"      expose: http\n", "",
			"names every unit",
		},
		"no name on the second unit": {
			"      name: web\n      expose: http\n", "",
			"names every unit",
		},
		"the same name twice": {
			"      name: web\n      expose: http\n", "      name: web\n",
			"is already the name of",
		},
		"no unit declares expose": {
			"      name: web\n", "      name: cache\n",
			"exactly one unit declares expose",
		},
		"every unit is expose none": {
			"      name: web\n      expose: none\n", "      name: cache\n      expose: none\n",
			"none of these units does",
		},
		"two units declare a face": {
			"      name: web\n      expose: http\n      port: 8080\n",
			"      name: cache\n      expose: mcp\n",
			"web and cache do",
		},
		"a name the shape refuses": {
			"      name: Web\n      expose: http\n", "      name: cache\n",
			"",
		},
		"a name over 32 characters": {
			"      name: " + strings.Repeat("w", 33) + "\n      expose: http\n", "      name: cache\n",
			"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(twoUnits(tc.first, tc.second)))
			if err == nil {
				t.Fatal("the manifest was accepted")
			}
			var invalid *manifest.ErrInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("error is %T, want ErrInvalid", err)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal says %q, want it to say %q", err.Error(), tc.says)
			}
		})
	}
}

// A Package of one unit is what it was: it may leave its unit unnamed, and an
// exposure it does not declare is none, which is how every manifest written
// before M12 reads.
func TestOneUnitIsUnchanged(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: quiet
description: One unnamed unit that declares no exposure at all, as a job does.
deploy:
  units:
    - type: container
      build: .
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	units, err := m.Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("the manifest carries %d units, want one", len(units))
	}
	if units[0].Name != "" {
		t.Errorf("the unit is named %q, want the Package to be what it is called", units[0].Name)
	}
	if units[0].Expose != manifest.ExposeNone {
		t.Errorf("the exposure is %q, want none", units[0].Expose)
	}
}

// A folder that is not a Package has no units and is not a manifest that is
// wrong: visibility is the whole of what it declares.
func TestUnitsOfAFolderThatIsNotAPackage(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: notes
description: A folder of writing, which carries no deploy block at all.
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	units, err := m.Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) != 0 {
		t.Errorf("the folder carries %d units, want none", len(units))
	}
}

// env was renamed environment, so the old spelling is an unknown field rather
// than a block that is quietly dropped: a manifest that kept it would start a
// container without the variables it declared, see PLAN.md section 5.6.
func TestEnvIsNotAField(t *testing.T) {
	_, err := manifest.Parse([]byte(`name: app
description: One unit that spells its environment the way compose stopped spelling it.
deploy:
  units:
    - type: container
      build: .
      expose: none
      env: { LOG_LEVEL: debug }
`))
	if err == nil {
		t.Fatal("a manifest with env was accepted")
	}
	var invalid *manifest.ErrInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("error is %T, want ErrInvalid", err)
	}
}

// The command a unit declares is read as the words it wrote, and a unit that
// declares none runs the command of its image.
func TestUnitCommand(t *testing.T) {
	m, err := manifest.Parse([]byte(unitWith("      command: [node, server.js, --port, \"8080\"]\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	units, err := m.Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if got := strings.Join(units[0].Command, " "); got != "node server.js --port 8080" {
		t.Errorf("the command is %q, want the words the unit declared", got)
	}
	// The shapes a command may not have, each of them what the daemon refuses
	// on the way in as well, so fs_write and proc_run agree.
	for name, block := range map[string]string{
		"an empty list":               "      command: []\n",
		"an empty word":               "      command: [node, \"\"]\n",
		"a word too long":             "      command: [\"" + strings.Repeat("a", 4097) + "\"]\n",
		"too many words":              "      command: [" + strings.Repeat("word, ", 64) + "word]\n",
		"a word that is not a string": "      command: [7]\n",
	} {
		if _, err := manifest.Parse([]byte(unitWith(block))); err == nil {
			t.Errorf("a manifest with %s was accepted", name)
		}
	}
}
