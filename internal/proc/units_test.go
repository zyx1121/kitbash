package proc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
)

// twoUnitManifest is the Package M12 is about: a face and a unit behind it,
// which the manifest layer reads whole and this runner does not yet start.
const twoUnitManifest = `name: board
description: A counter and the cache it keeps its count in, two units of one Package.
deploy:
  units:
    - name: web
      type: container
      build: .
      expose: http
      port: 8080
    - name: cache
      type: container
      image: docker.io/library/redis@sha256:` + redisDigest + `
      command: [redis-server, --save, "60 1"]
`

const redisDigest = "1111111111111111111111111111111111111111111111111111111111111111"

// The manifest layer reads every unit, and the built in runner runs one
// container and not a pod. A Package of several units is therefore refused
// whole rather than started as its first unit, which would be a Process that
// is half of what the member wrote, see PLAN.md section 5.6.
func TestRunningSeveralUnitsIsRefused(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	f.build(folder, "board")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Package of two units was started")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "composition of several units is not supported yet") {
		t.Errorf("the refusal says %q, want it to say composition is not supported yet", prob.Detail)
	}
	if len(f.runner.Runs) != 0 {
		t.Error("the runtime was asked to start the first unit of a Package it cannot run whole")
	}
	if n := len(f.daemon.Registrations()); n != 0 {
		t.Errorf("kitbashd holds %d registrations for a Package that was refused", n)
	}
}
