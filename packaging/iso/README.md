# kitbashOS ISO

An installable Alpine 3.23 image that boots as a kitbash host, see PLAN.md
section 4. The ISO carries the `kitbashd` apk and every package
`deploy/install.sh` needs, so the first boot works with no network at all.

The version is `pkgver` in `packaging/apk/APKBUILD` and nowhere else. The
output is `kitbash-<pkgver>-x86_64.iso` or `kitbash-<pkgver>-aarch64.iso`. Both
are built natively, one per release runner, and never under emulation.

## Files

| File | What it is |
|------|------------|
| `mkimg.kitbash.sh` | mkimage profile `kitbash`, based on `profile_virt`: virt kernel, x86_64 and aarch64, serial console, the apks list |
| `genapkovl-kitbash.sh` | the apk overlay: hostname, DHCP, OpenRC runlevels, the kitbashd signing keys, the firstboot script |
| `answers` | setup-alpine answerfile for an unattended sys install to `/dev/vda` |
| `build.sh` | builds the ISO from a kitbashd apk |
| `smoke.sh` | boots an ISO in qemu and checks that it came up as a kitbash host |

## Build

```sh
packaging/iso/build.sh <path to kitbashd apk> <outdir> [arch]
```

The architecture is the third argument, `$KITBASH_ARCH`, or this host's
`uname -m`, and it has to be the apk's. On an Alpine 3.23 host with:

```sh
apk add alpine-sdk alpine-conf xorriso squashfs-tools grub grub-efi \
        mtools dosfstools git
apk add syslinux grub-bios          # x86_64 only, and only there
```

`syslinux` and `grub-bios` do not exist for aarch64, which is why the profile
asks for them only on x86_64: an x86_64 image is BIOS or UEFI and an aarch64
image is UEFI with `grub-efi` alone. The same split decides what `setup-disk`
puts on the disk during the unattended install.

What it does: indexes the apk into a local repository and signs the index,
clones aports `3.23-stable` shallow and sparse (`scripts/` only), copies the
profile and the overlay generator into `aports/scripts/`, and runs
`mkimage.sh --profile kitbash --tag <pkgver>`.

Working directory is `$HOME/iso-work`, or `$KITBASH_ISO_WORK`. The aports
checkout and the mkimage cache live there, so a second build is much faster.

It runs as a normal user with an abuild key, and as root in an `alpine:3.23`
container, where it creates a build user and re-runs itself as them. With no
key it creates one with `abuild-keygen -a -n -q`; set `PACKAGER_PRIVKEY` to use
a specific one. That key signs the modloop and the ISO's own package index, and
the overlay trusts it on the installed system, so the ISO stays self contained
whichever key it was built with. The kitbashd apk may be signed by a different
key: `build.sh` puts `packaging/apk/keys/*.rsa.pub` into `/etc/apk/keys` when
it can, and passes `--hostkeys` so the image trusts them too.

In CI, keep `<outdir>` in the workspace. `/root` is mode 0700 and the build
user cannot write under it.

## Smoke test

```sh
packaging/iso/smoke.sh <iso> [serial log]
```

Boots the ISO in qemu and waits for `kitbash-firstboot-ok` on the serial
console, then always prints the tail of the log. Needs no root. A pass means
`deploy/install.sh` ran, `kitbashd` and `kitbash-mcp` answered `--version` and
OpenRC reports the service started.

The architecture comes from the ISO's own name, or from `$KITBASH_ARCH`, and it
decides everything else:

| | x86_64 | aarch64 |
|---|---|---|
| binary | `qemu-system-x86_64` | `qemu-system-aarch64` |
| machine | the default | `-M virt`, which has no BIOS |
| firmware | the built in SeaBIOS | AAVMF or `QEMU_EFI.fd`, found on the host |
| media | `-cdrom` on IDE | a `scsi-cd` on a `virtio-scsi-pci` bus |
| console | `ttyS0` | `ttyAMA0` |
| default timeout | 300 s | 900 s |

`KITBASH_SMOKE_TIMEOUT` overrides the timeout. Acceleration is KVM when
`/dev/kvm` is writable and the guest is this machine's architecture, `hvf` on an
Apple silicon Mac, and TCG otherwise, which is what GitHub's runners get: they
have no nested virtualisation, on either architecture.

## What the image does on first boot

1. The initramfs unpacks the overlay and apk installs `/etc/apk/world`,
   which is `alpine-base` and `kitbashd`, from the ISO's own repository.
2. OpenRC starts the base services, `cgroups` in sysinit because kitbashd
   needs the unified hierarchy, then `sshd` and `local`.
3. `/etc/local.d/kitbash-firstboot.start` runs `sh /usr/share/kitbash/install.sh`
   once, guarded by `/var/lib/kitbash/.firstboot-done`, and prints
   `kitbash-firstboot-ok` when the host is a kitbash host.

Nothing in the image is human facing. No passwords, no extra users, no getty
beyond the console: `PermitRootLogin prohibit-password` is what install.sh
writes, and the console is the break glass path of PLAN.md 4.5. The first
admin is created there with `kitbash-adduser`.

## Install to disk

The ISO publishes the answerfile at `/media/cdrom/kitbash/answers`. mkimage
has no hook for extra files, so the profile carries its own section that
copies it to `/kitbash/answers` at the ISO root. From the live console:

```sh
setup-alpine -f /media/cdrom/kitbash/answers
```

It erases `/dev/vda`, installs in `sys` mode and asks nothing. `setup-disk`
copies the running `/etc`, so the kitbash configuration the firstboot script
wrote, the sshd drop in, the groups and the runlevels, all carry over.

## Booting the aarch64 image on Apple silicon

There is no arm Proxmox host here, so the aarch64 image's acceptance is the CI
smoke boot plus one boot by hand on a Mac. Both are the same test: the serial
console has to print `kitbash-firstboot-ok`.

With QEMU, which is what `smoke.sh` drives:

```sh
brew install qemu
packaging/iso/smoke.sh kitbash-0.3.0-aarch64.iso
```

That takes the `hvf` path, so it runs at native speed. For scale, CI boots the
same image under TCG on an arm runner in 93 s. The same boot by hand, to sit at
the console afterwards:

```sh
qemu-system-aarch64 \
  -machine virt,accel=hvf -cpu host -m 2048 -smp 2 \
  -nographic -no-reboot \
  -bios "$(brew --prefix qemu)"/share/qemu/edk2-aarch64-code.fd \
  -drive if=none,id=cd0,file=kitbash-0.3.0-aarch64.iso,format=raw,media=cdrom,readonly=on \
  -device virtio-scsi-pci,id=scsi0 -device scsi-cd,drive=cd0,bus=scsi0.0 \
  -nic user,model=virtio-net-pci
```

`-nographic` puts the guest's `ttyAMA0` on the terminal, which is the console
the image is built for; `Ctrl-a x` quits. The image is UEFI only, so the
firmware argument is not optional: without it QEMU starts an aarch64 machine
with nothing to boot from and the screen stays empty.

With UTM, for a VM that stays around: New, **Virtualize**, **Linux**, no kernel
image, and pick the ISO as the boot image. UTM's virtualized Linux VMs on Apple
silicon are ARM64 with UEFI already, which is what the image expects. Set memory
to 2 GB or more, and turn on the serial console in Devices if the display stays
blank: the kernel command line names `ttyAMA0` first and `tty0` second. To
install it to the VM's disk afterwards, the answerfile path is the same one
x86_64 uses, `setup-alpine -f /media/cdrom/kitbash/answers`, and it writes to
`/dev/vda`.

## Gotchas

- **mkimage caches by checksum, and reads `$apkovl` relative to the current
  directory.** Run `mkimage.sh` from anywhere but `aports/scripts` and the
  checksum of `genapkovl-kitbash.sh` is empty, so a changed overlay generator
  silently reuses the cached overlay. `build.sh` cds there first.
- **`--extra-repository` prints a deprecation warning** and still works. It is
  the only way to add a repository that is not in `--repository`.
- **`PACKAGER_PRIVKEY` is not optional.** `profile_base` sets
  `modloop_sign=yes`, and mkimage refuses to build without a key.
- **OpenRC sends `local.d` output to `/dev/null`** unless `rc_verbose` is set.
  The firstboot script writes to `/dev/console` itself, which is also why the
  profile puts `console=ttyS0` last on the kernel command line: the last
  console is the one `/dev/console` points at.
- **kitbashd must not depend on `local`.** It used to be `after local`, for
  the rootless prerequisites that hook installs, and OpenRC will not start a
  service while one it is after is still starting: `rc-service kitbashd
  restart` inside `install.sh`, which the firstboot script runs from
  `local.d`, then blocked forever and took the boot with it, because busybox
  init only starts the gettys after `::wait:/sbin/openrc default` returns.
  Those prerequisites are `/usr/share/kitbash/rootless-prereqs.sh` now and
  the service script runs them from `start_pre`, so the firstboot script
  calls `install.sh` inline (issue #88).
- **mkimage cannot run as root.** It calls `apk add --initdb --no-chown`, and
  apk-tools 3 answers `--usermode not allowed as root`. `build.sh` creates
  `kitbash-build` (override with `KITBASH_ISO_USER`) and re-runs itself as
  that user, copying `PACKAGER_PRIVKEY` into their home first. Everything
  after that, including the output ISO, is owned by the build user.
- **An apk signed by a key the build host does not trust fails the build**
  with `UNTRUSTED signature` in the apks section, not in the local repository
  index. mkimage seeds its apk root from `/etc/apk/keys` plus
  `$PACKAGER_PRIVKEY.pub` and nothing else, which is why `build.sh` adds the
  repo's keys there and passes `--hostkeys`.
- **`apk index` warns "No provider for the dependencies"** for the local
  repository, because kitbashd's dependencies live in the Alpine repositories
  and not in it. Harmless.
- **aarch64 has no `syslinux` and no `grub-bios`.** Ask for either in the
  profile's `apks` and the image does not resolve at all. mkimage's own
  `section_syslinux` already returns early off x86, so the ISO is EFI only
  there: one El Torito entry pointing at the FAT image with
  `efi/boot/bootaa64.efi` in it, and no isohybrid MBR.
- **The aarch64 serial console is `ttyAMA0`, not `ttyS0`.** It is a PL011 on
  qemu's `virt` machine and on every arm server. The profile puts the right
  one on the kernel command line and exports `KITBASH_ISO_CONSOLE` so the
  overlay generator, which runs as a separate process under fakeroot, puts the
  getty on the same line. Get this wrong and the boot is silent: it works, and
  nothing can be seen or typed.
- **The mkimage cache is not fully keyed by architecture.** Only the kernel
  and apks sections carry `$ARCH`; the apk overlay and the answerfile section
  do not, and the overlay differs between the two architectures. `build.sh`
  gives mkimage `--workdir $KITBASH_ISO_WORK/mkimage-<arch>` so the two builds
  never share a cache.
- **apk asks the repository for the canonical file name.** A release cannot
  carry two files called `kitbashd-<ver>-r0.apk`, so they are
  `kitbashd-<ver>-r0.x86_64.apk` and `kitbashd-<ver>-r0.aarch64.apk`. apk
  still resolves `kitbashd-<ver>-r0.apk` out of the ISO's local repository
  whatever the index says, so `build.sh` copies the apk in under that name.
- **In a container as root**, nothing needs doas, but `git` needs the aports
  checkout in `safe.directory` when the clone is owned by another user;
  `build.sh` adds it. Run the container privileged enough for `fakeroot`,
  which mkimage uses to build the overlay.
