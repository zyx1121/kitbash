package sysusers

import "testing"

// The inspect below is what podman 5.7.0 printed for a container started with
// --mount type=bind,src=...,dst=...,ro=true,bind-nonrecursive,nosuid,nodev,noexec,
// trimmed to the keys kitbash reads. HostConfig.Binds carries the same mounts
// as one string each, options and all; the top level Mounts array is already
// the three fields kitbash cares about, which is why it is the one decoded.
const mountedInspect = `[{
 "ImageName": "sha256:abc",
 "Config": {"Env": ["PATH=/usr/bin"], "Labels": {"kitbash.id": "p-alpha"}},
 "HostConfig": {
  "CgroupParent": "/kitbash/loki/p-alpha",
  "RestartPolicy": {"Name": "always"},
  "PortBindings": {},
  "Binds": ["/home/loki/notes:/files/notes:ro,bind,nosuid,nodev,noexec,private"]
 },
 "Mounts": [
  {"Type": "bind", "Source": "/home/loki/notes", "Destination": "/files/notes",
   "Options": ["bind", "nosuid", "nodev", "noexec"], "RW": false, "Propagation": "private"},
  {"Type": "bind", "Source": "/home/loki/out", "Destination": "/files/out",
   "Options": ["bind", "nosuid", "nodev", "noexec"], "RW": true, "Propagation": "private"},
  {"Type": "volume", "Name": "scratch", "Source": "/var/lib/containers/x", "Destination": "/scratch", "RW": true}
 ]
}]`

func TestContainerConfigReadsTheBindMounts(t *testing.T) {
	config, err := containerConfig(mountedInspect)
	if err != nil {
		t.Fatalf("containerConfig: %v", err)
	}
	// The volume is the runtime's own and has no folder of Files behind it, so
	// it is not one of these.
	if len(config.Mounts) != 2 {
		t.Fatalf("the configuration holds %+v, want the two bind mounts", config.Mounts)
	}
	if config.Mounts[0].Source != "/home/loki/notes" || config.Mounts[0].Target != "/files/notes" ||
		!config.Mounts[0].ReadOnly {
		t.Errorf("the first mount is %+v, want the notes folder read only", config.Mounts[0])
	}
	if config.Mounts[1].Source != "/home/loki/out" || config.Mounts[1].ReadOnly {
		t.Errorf("the second mount is %+v, want the out folder read write", config.Mounts[1])
	}
}

// A container with no mount at all reads back with none, which is every
// container started before mounts existed.
func TestContainerConfigWithoutMounts(t *testing.T) {
	config, err := containerConfig(`[{"ImageName":"sha256:abc","HostConfig":{"CgroupParent":"/kitbash/loki/p"}}]`)
	if err != nil {
		t.Fatalf("containerConfig: %v", err)
	}
	if config.Mounts != nil {
		t.Errorf("the configuration holds %+v, want no mounts", config.Mounts)
	}
}
