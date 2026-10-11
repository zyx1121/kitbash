# kitbash

Agent operating system: `kitbash-mcp` (the MCP surface that sshd runs per member) and `kitbashd` (the resident daemon), both in Go, packaged as an Alpine apk and ISO. [PLAN.md](PLAN.md) is the one design document. Read it and [README.md](README.md) before you change anything.

## Rules

- A member gets the MCP surface and never a shell. Keep `ForceCommand` and the sshd rule.
- `kitbash-mcp` ignores `KITBASH_ROOTS` and `KITBASH_SOCKET` inside an SSH session. Keep it that way, so a member never chooses them.
- Port 4318 accepts Process traffic on loopback only. Do not widen the nftables rule.
- Member secrets stay outside the database, root only, so backups carry none.
- Third-party tools arrive as Packages. Do not install them on the host.
- The version has one source, `pkgver` in `packaging/apk/APKBUILD`. Use `make bump`, never hand edits.
- Repository documents and code comments are English.

## Where to run things

In Loki's environment, the MacBook is for editing and git only. Run `make build`, `make test` and `make lint` on `ssh sandbox` in a login shell (`bash -lc`). Edit in a scratchpad worktree, not in the shared iCloud clone. The apk and the ISO build on Alpine in CI.

## Verify

```sh
make lint
make test
make check-version
```

`make check-version` fails when a README, a translation, `docs/install.md` or `packaging/iso/README.md` names a version other than `pkgver`. For a change to the host install, run `install.sh` on a test VM and list the tools over SSH.

## Release

`make bump VERSION=X.Y.Z` writes the version everywhere and commits `Release X.Y.Z`. Push the branch, open a PR and merge it. `release.yml` builds, boots and publishes.
