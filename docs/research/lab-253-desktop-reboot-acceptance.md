# Real Chrome and Slack acceptance — 2026-10-15

[LAB-253](https://linear.app/riotbox/issue/LAB-253/complete-real-chrome-and-slack-lifecycle-and-reboot-acceptance)
completed the opt-in real desktop matrix in a fresh disposable KVM guest:
15 passed cases, one explicitly unsupported protocol case, and two UI
capabilities absent from the inspected empty Slack profile. Two regular
acceptance reboots restored the original application identities and ordered
layout. The runner wrote its passing result only after closing its VM and
removing its private disk, keys, profiles, screenshots and control directory.

This supplements the historical [LAB-95 connected fixtures](../desktop-application-acceptance.md).
It changes test tooling and evidence; production lifecycle, identity, trust,
launch guards and timeouts are unchanged.

## Artifact and environment

| Item | Recorded value |
| --- | --- |
| Run UUID | `736bbad0-d2a5-481f-8149-c4bf7e5b4136` |
| Candidate commit | `961bb89508f0f9e16000d0b97646d84714fa4a6b`, clean build |
| Candidate SHA-256 | `8b1c3a6cbffb3b5d6189434cb49b79ccc42e9b85e19405a093c95285cb83dc5e` |
| Structural `result.json` SHA-256 | `e3b5645fef8f4e144908e9f30fcab3c410f06e2c0e14a808b5432db06da63c1c` |
| Chrome | Native vendor Debian payload, `155.0.8059.79` |
| Chrome payload SHA-256 | `b7726070ca707965e9a17b38e4f15ba283bf6f65c339626c092862af7b19badc` |
| Slack | User Flatpak `com.slack.Slack`, `4.52.178` |
| Slack ref | `app/com.slack.Slack/x86_64/stable` |
| Slack commit | `a12a31786186231c99d2b4703f9017be95cf18e7eb452d7ab5b338f272b26f25` |
| Slack remote | Ordinary GPG-verified Flathub, `https://dl.flathub.org/repo/` |
| Guest | Kernel `7.2.9-arch1-1`, Sway `1:1.12-4`, systemd `262`, Flatpak `1.18.4`, Xwayland `24.1.14-1` |
| Other tooling | Python `3.14.8-1`, Herdr `0.9.2`, host QEMU `11.1.2` |
| Observer SHA-256 | `8cb37a4562dd716758f5759f8cd187563841d935b521f35cbcc577261ae3c077` |

The base Arch image and Herdr payload retain the pinned digests in
[`assets.json`](../../scripts/vm-reboot/assets.json). The guest had 6 GiB RAM,
two virtual CPUs and a 32 GiB sparse overlay. It used a real tty1 PAM/logind
login, Bash, private D-Bus, headless Sway and the actual candidate daemon plus
one-shot restore. Every application profile and workspace 98 window belonged
to this fixture. No signed-in profile or host application installation was used.

## Actual protocol, trust and UI results

| Cases | Result |
| --- | --- |
| Vendor Chrome stock entry | Its main group had one `StartupWMClass`; production catalog/registration accepted it. A separate disposable duplicate-key entry was omitted/rejected. |
| Chrome Wayland and XWayland | Both mapped and registered. Native app ID was `google-chrome`; X11 class was `Google-chrome`, with the fixed disposable profile instance. |
| Slack Wayland | Mapped and registered with app ID and sandbox app ID `com.slack.Slack`, engine `org.flatpak`, and a sandbox instance. |
| Slack XWayland | Mapped with class `com.slack.Slack` and instance `com.slack.slack`, but no sandbox identity. Registration was rejected: explicit unsupported case. |
| Chrome and Slack graphical cancel/approve | Four real swaynag surfaces were inspected and clicked. Cancellation preserved registrations/trust; approval invoked the candidate callback and produced one context. Repeated registration returned that same context without another prompt. |
| Chrome material reapproval | Changed fixture launcher execution material required reapproval and caused zero browser starts while blocked. Real cancel preserved old approval; real approve replaced the digest. The approved normal GIO route subsequently started Chrome once. |
| Chrome multiwindow ambiguity | Actual UI created a second eligible window. Removing only the known original fixture mark produced canonical ambiguous presence, no anchor and placement-only capture. Slack's anchor remained intact; no additional application main started. |
| Slack multiwindow | Not exposed by the inspected empty sign-in UI and File/Window menus. Input automation was available; its absence was not used as a non-exposure result. |
| Chrome profile chooser | Actual avatar → Manage Chrome profiles → existing Your Chrome tile flow. Canonical presence stayed `present`; matching windows changed `1→2→1`, total windows `2→3→2`, over 29 samples. The same marked main and desired-open intent survived. No main-window gap was observed. |
| Slack picker | Not exposed after inspecting the empty sign-in UI and its File/Window/Go menus. |

The XWayland Slack probe used a guest-only per-launch Wayland-socket exclusion
and X11 option to exercise that backend. Its normal native launcher and
installation were unchanged. No class-based sandbox-identity fallback was added.

The GUI/reboot Chrome context used an explicitly approved disposable user-local
entry and real GIO; Slack used the normal user Flatpak launcher. Stock-entry
compatibility is a separate result. The earlier Arch entry with duplicate
`StartupWMClass` remains unsupported: acceptance does not silently repair trust
metadata or establish compatibility for every distribution's stock file.

## Real close policy and shutdown protection

Both Follow cases subscribed before closing the fresh exact marked anchor,
observed its actual close event, and retained verified candidate/logind health
through absence beyond the unchanged 2,000 ms production grace.

| Application | Follow absence | Follow outcome | Pinned absence after daemon restart / Sway reload |
| --- | --- | --- | --- |
| Chrome | 3,806 ms, three stable samples | Active, desired-open false; zero new mains | 2,053 / 2,062 ms; desired-open true; zero new mains |
| Slack | 3,707 ms, three stable samples | Active, desired-open false; zero new mains | 2,066 / 2,059 ms; desired-open true; zero new mains |

Pinned checks retained the same Sway process (`540`) and socket through each
daemon restart and reload. They observed absence separately after both actions;
no restore/reopen call supplied the no-watchdog result. Each application was
then intentionally reopened and returned to Follow/desired-open before the
reboot baseline. This makes shutdown protection observable instead of relying
on Pinned to suppress close inference.

## Two acceptance reboots

One acknowledged regular setup reboot first settled the upgraded guest OS:
`8ea1e352-3eb9-47db-b889-93bfcc58a44a` → baseline boot below. It happened before
registration and the matrix and is recorded separately from these two required
acceptance reboots. Every transition used the guest systemd timer →
`systemctl reboot` path; no forced reset supplied acceptance.

| Observation | Boot UUID | Daemon / inhibitor PID | Chrome / Slack mains | Classified Slack helpers | Stable samples |
| --- | --- | --- | --- | --- | --- |
| Original baseline | `96d31e98-daf8-4cc9-891c-f6860ad37768` | `6059` | `1 / 1` | `1` | `3` |
| Acceptance reboot 1 | `07be7c7c-08e7-41cf-ac86-0eb08c28910f` | `540` | `1 / 1` | `1` | `3` |
| Acceptance reboot 2 | `440cdfaa-60a6-40f4-abf6-8672df5b47f1` | `533` | `1 / 1` | `1` | `3` |

All three observations retained two active Follow/desired-open contexts,
one uniquely marked native window per application, workspace 98 and the same
ordered tabbed Chrome/Slack topology in live and stored captures. There were
no managed staging windows or extra/floating/fullscreen windows. Each boot
matched the original baseline's candidate, versions, application identities and
source proof. The real `shutdown:sleep` delay inhibitor belonged to that boot's
attributed candidate PID and UID `1000`, in the active tty1 login session.

## What the launch counts prove

The bounded kernel exec/fork/exit observer checks event loss and tracks
incarnations, descendants, leader changes and PID reuse. Under
`fixed-normal-launchers-v1`, the pinned Slack entry/wrapper/helper chain counts
a successful main start. A positively observed Flatpak portal and helper-only
chain separately classifies a sandbox child executing the same Slack ELF.
Unknown Slack origins reject the run. Helper counts are measured, not assumed
constant; a second independent normal main chain counts even if it exits
before window sampling. Window count or a durable attempt row cannot replace
this measurement.

The exact Slack commit, installed manifest, entry, wrapper and helper hashes
and Zypak source commit are fixed in the pure evidence contract. The measured
root-owned portal hash was
`05d8586b247265f4dd1f651a0ed7569c37d1c393d282e361afc4ff2b14d653e1`
throughout. The pinned [Zypak wrapper](https://github.com/refi64/zypak/blob/693a71c5ffa80ec9c9ce2ae03b1ccc493c698e53/zypak-wrapper.sh)
and [portal child launcher](https://github.com/refi64/zypak/blob/693a71c5ffa80ec9c9ce2ae03b1ccc493c698e53/src/base/launcher.cc)
explain the separate helper route.

This is not general logical-process accounting. Filename-only events cannot
distinguish an arbitrary portal request for helper host mode from its child
route. Custom portal requests, altered launchers and optional spawn-latest mode
are outside this fixed empty-profile fixture. No runtime argv/environment was
read for classification. Public Chrome version metadata was cached before
tracing so that observing a version did not launch another Chrome ELF.

## Verification and limits

The successful run's schema checker independently returned `passed:true`,
`setup_reboots:1`, `guest_reboots:2`, `phase_count:18` and `managed_windows:2`.
The execution and privacy contract and reproduction command are in
[KVM reboot verification](../kvm-reboot-verification.md). Ordinary `make verify`
runs synthetic harness/observer checks and does not launch these applications.

Exploratory work and two earlier failed full attempts are not counted as
acceptance. One failed after Slack approval; another reached the original
baseline but failed the first postboot comparison. Both cleaned up. The earlier
baseline ran kernel `7.2.7`; the settled successful baseline ran `7.2.9`.
The missing earlier postboot observation prevents retrospectively proving its
precise failure cause. The harness now retains only fully schema-validated
postboot counterexamples, fixed error categories and bounded source locations.
Raw errors, browser logs, private state and screenshots remain excluded.

A subsequent read-only session-readiness wait covers SSH becoming available
before getty/Sway/routing/candidate health. Delayed-readiness and timeout tests
cover that addition; the full run above predates it. It retries only prerequisites,
never effectful phases. Production source and the observer/counter were not
changed after the recorded run.

This result covers its exact build, empty profiles, structural identities,
outer placement/layout and observed lifecycle. It does not establish
authenticated Slack/browser content, browser-tab recovery, a main-window gap,
generic per-window reboot identity, physical notebook suspend/power behavior,
AppArmor enforcement, focus, proportions or floating geometry. App-owned data
remains app-owned. The LAB-89, LAB-93 and LAB-94 feature gates remain separate.
