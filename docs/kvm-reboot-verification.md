# Automated KVM reboot acceptance

`make vm-reboot-check` creates one disposable Arch Linux KVM guest, installs the
candidate, and verifies two normal guest reboots. It never reboots the host or
reads the host's session database, Sway socket, Herdr sessions, or agent history.
This is a development-only integration test; no VM tool becomes a runtime or
package dependency of sway-session.

The first real run exposed a startup interaction between the one-shot restore
report and normal window adoption: report-driven layout retry set eligibility
before adoption could authorize the new window's automatic focus. The daemon
then cancelled reconstruction and captured the default split layout. Runtime
and private-compositor regressions cover pending CLI reports, late mapping,
reversed window creation and explicit user cancellation. Report-driven layout
retry now waits for an already marked live window or application anchor; the
normal adoption path handles fresh windows first.

The observer also requires the candidate daemon's actual shutdown/sleep delay
inhibitor after each reboot. This caught a logind initialization race: removing
an unrelated SSH session before the daemon resolved its own login identity was
mistaken for its own logout. Early removal signals are now attributed after
identity resolution with bounded storage and sender validation. Own-session
loss still disables the guard. A missing inhibitor remains an acceptance
failure even when the windows and layout otherwise appear correct.

## Requirements and execution

The host must be Linux x86_64 with read/write access to `/dev/kvm`, Python 3.11+, Go from `go.mod`, QEMU, `qemu-img`, `cloud-localds`,
`ssh`, `ssh-keygen`, and curl 8.4.0 or newer.
Missing prerequisites fail explicitly; they do not count as successful reboot
coverage. Host privilege elevation and service changes are not performed by the
runner. GitHub Actions grants KVM access only on its disposable runner.

```sh
GOTOOLCHAIN=go1.26.5 make vm-reboot-check \
  VM_ARGS='--output /tmp/my-new-sway-session-vm-evidence'
```

The output directory must be new and outside the checkout. The runner downloads
its pinned official Arch cloud image and Herdr 0.9.2 from
[scripts/vm-reboot/assets.json](../scripts/vm-reboot/assets.json), verifies
SHA-256 with a bounded HTTPS download, creates a private writable overlay and
cloud-init seed, and provisions
Sway, Alacritty, Mesa, fonts, systemd/D-Bus, Polkit and observation tools in the
guest.
Arch package versions are recorded by the fixture; repository updates make the
guest package installation current rather than a fully reproducible package
snapshot. Update asset URLs and digests together in a reviewed change when the
pinned distribution image ages out of its mirror.

To reuse the exact pinned cloud image and an existing Herdr executable:

```sh
GOTOOLCHAIN=go1.26.5 make vm-reboot-check \
  VM_ARGS='--image /absolute/arch.qcow2 --herdr /absolute/herdr --output /tmp/new-vm-evidence'
```

An optional `--candidate /absolute/sway-session` tests an existing Linux amd64
artifact. Otherwise the runner builds the current checkout with its real commit
and dirty flag. Candidate content SHA-256 and embedded build metadata are
recorded and checked after both reboots. The canonical observer is built from
this checkout. There is no installed-host-binary fallback.

The VM uses 2 vCPUs, 2 GiB RAM and a 12 GiB sparse disposable disk. Its clock
starts from QEMU's normal host-UTC RTC. The guest-only network time-wait service
is masked during cloud-init so unavailable NTP cannot indefinitely block
provisioning over user-mode networking. This does not change the host clock,
shutdown monitor, logind or inhibitor behavior. Its SSH
forward listens only on an ephemeral localhost port. Control uses a generated
per-run SSH key and a private known-hosts file; no host SSH configuration,
credentials, shared home directory or host state mount enters the guest. Guest
reboot commands are sent only through the connection to the owned QEMU process,
after checking its run marker, KVM identity and boot ID. QMP additionally confirms
that KVM acceleration is enabled. Cleanup stops only that owned QEMU child and
removes its disposable disk, seed and keys. Subprocess output and the VM console log are capped while they are read;
asset downloads have a 600-second transfer deadline and a size limit.
Bounded diagnostic evidence remains
in the requested output directory, including on failure.

## Observable acceptance

The fixture uses an automatically logged-in disposable account and a real
PAM/logind user session. Its Sway configuration starts the candidate daemon and one-shot `restore`
at login, matching the normal integration. The initial baseline creates two real persistent Herdr terminals through
`sway-session terminal --new`, arranges them on workspace 98 in tabbed layout,
and waits for the daemon to persist the same ordered structure.

The host schedules `systemctl reboot` inside that guest twice with a three-second
guest systemd timer. Each scheduling command must return success before SSH is
closed by shutdown, and the immediately preceding boot ID must still match the
last observation. Failed SSH requests and unexpected reboots fail the test. Its persistent
state and VM disk remain unchanged between boots; no snapshot rollback, forced
hypervisor reset, injected shutdown event or manual layout repair is used.
After each reboot, observation waits for the normal login/autostart/restore
path. It never starts or repositions the managed windows itself.

Pass requires three different guest boot IDs, two acknowledged normal reboot
requests, unchanged active context IDs,
the same ordered tabbed structure on workspace 98 in both live Sway and stored
layout, no managed windows left in staging, a running daemon matching the
candidate, and a real logind shutdown delay inhibitor. Each restored state must match in three consecutive observations and is
compared with the **original baseline**. The observer uses the normal session
stores and canonical `CaptureLayout`; only incidental singleton wrappers are
normalized, while meaningful branching layouts and child order are retained.
Focus, proportions, floating geometry, agent conversation resume, pane working
directories and AppArmor enforcement are not covered by this scenario. It does
not claim that the user's notebook or its specific configuration was tested.

`before.json`, `after1.json`, `after2.json` and `result.json` hold the bounded
fixture evidence. A failure writes `failure.json` and bounded guest logs. The
source/runner test suite exercises rejection of lost layouts, changed child
order, missing contexts, staged windows, candidate mismatch and repeated boot
IDs with synthetic evidence. Those tests validate the checker; they are never
reported as actual KVM reboot results.

```sh
make vm-harness-check
python3 -B scripts/verify-vm-reboot.py --check-evidence /tmp/my-vm-evidence/result.json
```

The evidence-checking command validates an existing report. It cannot establish
that a new VM run occurred or make an arbitrary supplied report trustworthy.

## CI and release use

[KVM reboot acceptance](../.github/workflows/vm-reboot.yml) runs on affected pull
requests and can be started with `workflow_dispatch`. It fails when KVM is
unavailable. CI uploads only the selected bounded JSON/log evidence, never the
VM disk, cloud-init seed, SSH private key or guest state database.

Run it against the actual release candidate before publication and record its
artifact digest, commit, guest versions and result alongside `make verify` and
private-compositor evidence. A green source test or container restart does not
stand in for the two guest reboot observations.
