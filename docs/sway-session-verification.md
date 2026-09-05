# Sway Session verification

This document separates the repeatable automated gate from live compositor,
packaging, migration, and security checks. Never report a live check that was
not run.

## Automated gate

Run:

~~~sh
make verify
~~~

The gate covers:

- gofmt;
- uncached unit tests;
- race-detector tests;
- go vet and staticcheck;
- CGO-disabled production build;
- AppArmor policy structure;
- Bash, Zsh, and Fish completion behavior;
- standalone source and package boundaries;
- GoReleaser, Arch, install-path, and release-workflow metadata;
- git whitespace checks; and
- the version-1 title-indicator golden wire fixture.

CI additionally runs GoReleaser configuration validation. A release candidate
must also run a clean GoReleaser snapshot and inspect every archive, DEB, and
RPM so only sway-session and the documented integration assets are present.

## Standalone extraction checks

Confirm the Go package graph contains the sway-session command and exactly the
retained internal responsibilities, with no animator command or old module
imports:

~~~sh
sh scripts/check-standalone-boundary.sh
go list ./...
rg 'github\.com/marang/sway-title-animator' --glob '*.go'
test ! -e cmd/sway-title-animator
~~~

The last rg command must produce no output. User-facing historical references
may name sway-title-animator only to explain independence, shared mark
compatibility, preserved Git history, or the package-ownership transition.

The v1 presentation mark fixture must stay byte-identical in both sibling
repositories:

~~~sh
cmp ../sway-session/internal/titleindicator/testdata/v1.json \
  ../sway-title-animator/internal/titleindicator/testdata/v1.json
go test ./internal/titleindicator
~~~

The fixture is authoritative. Do not normalize or regenerate it independently
on one side.

## State and migration checks

All probes use disposable XDG roots. Never point a test command at live user
state.

Create a fresh schema-1 database:

~~~sh
probe=$(mktemp -d)
XDG_STATE_HOME="$probe" sway-session register \
  --id 88888888-8888-4888-8888-888888888888 \
  --session fresh-schema-probe --cwd /tmp --label "Fresh schema probe"
test -f "$probe/sway-session/state.sqlite3"
test "$(sqlite3 "$probe/sway-session/state.sqlite3" \
  'PRAGMA user_version')" -eq 1
sqlite3 "$probe/sway-session/state.sqlite3" \
  'PRAGMA journal_mode; PRAGMA foreign_key_check; PRAGMA quick_check;'
~~~

Expect wal, no foreign-key rows, and ok. Verify the database and every present
WAL, SHM, or journal sidecar are regular, single-link, current-owner files with
mode 600. Stop every process using the disposable root before removing only
that root.

The pre-release JSON migration remains source-preserving. In a second
disposable root, prepare recognized legacy contexts.json, layout.json,
application-runtime/application-session.json, and
terminal-runtime/terminal-activity.json fixtures. Confirm ordinary state
opening fails closed while the database is absent, run terminal manage action
m, then verify:

- all valid rows were imported in one transaction;
- stale activity or launch rows whose context is absent were reported and
  skipped;
- every source JSON file is unchanged;
- repeating migration returns verified success without duplication; and
- once state.sqlite3 exists, the legacy files are ignored and never recreated.

LAB-119 introduces no path, schema, or migration change. Existing live state
must not be opened or rewritten merely to verify the repository split.

## Real Sway and Herdr check

Use a private compositor/socket, disposable XDG config, state, and runtime
roots, and workspace 98 or higher. Never create, move, close, restore, or purge
a test window on a single-digit workspace. Enable pane history in the
disposable Herdr config and use a short enough root for Herdr Unix socket path
limits.

1. Build the candidate sway-session binary.
2. Start the daemon against the private Sway socket.
3. Create a fresh persistent terminal with terminal --new and record only its
   disposable context UUID.
4. Confirm one typed terminal maps and receives the stable context mark.
5. Let the daemon capture workspace and layout into state.sqlite3.
6. Stop the daemon, close only that exact test window, and run one-shot restore.
   Confirm the terminal maps again with the same UUID and Herdr session.
7. Start the daemon and confirm it places the window on the saved high-numbered
   workspace and converges its outer layout.
8. Archive/activate and then exact-ID purge the disposable context.
9. Confirm the registry is empty, stop every test process, and remove only the
   disposable roots and high-numbered workspaces.

One-shot restore proves launch or mapping. Saved workspace placement and layout
require the daemon; never claim placement from a daemon-free mapping test.

Also exercise:

- absent → mapped → absent during both terminal stability checks;
- reuse after closing the outer adapter without restarting an occupied agent;
- role initialization failure followed by idempotent exact-context retry;
- rejection of a conflicting persisted adapter or working directory;
- lifecycle lock serialization across terminal, restore, and broker entry
  points;
- more than 128 stored contexts without a registry-capacity error;
- rotating placement/indicator batches after a completely rejected batch;
- application preflight rotation beyond its two-candidate pass bound; and
- layout re-observation after every mutation and after bounded yield.

### LAB-119 extraction evidence

On 2026-09-05, source-built binaries were exercised against Sway 1.12,
Alacritty 0.17.0, and Herdr 0.8.2 on a private headless compositor with
isolated XDG roots and workspace 98. No live user state or single-digit
workspace was used.

- sway-session daemon created the SQLite state and remained alive independently
  of sway-title-animator.
- terminal --new created one marked persistent terminal context.
- With both daemons stopped, one-shot restore remapped the same context and
  Herdr session, proving launch/mapping independence.
- After capture, the daemon was restarted from another high-numbered workspace;
  restore remapped the terminal and daemon reconciliation returned it to saved
  workspace 98 without an animator.
- Exact purge emptied the disposable registry.
- All test processes and disposable contexts were removed.

The isolated harness first exposed test-setup mistakes—a too-long Herdr socket
root and a missing pane-history setting. Retrying with the packaged Herdr
template and a shorter private root passed; neither was a product failure.

The same extraction also passed source-built Arch package transitions from the
combined sway-title-animator 0.9.3 package in disposable pacman roots: both a
single transaction installing animator 0.10.0 plus sway-session 0.1.0 and a
sequential animator upgrade followed by sway-session installation. Neither
required an overwrite flag. Package ownership checks assigned each binary to
its respective package. These were isolated package-manager checks, not a
change to the running workstation's installed packages.

GoReleaser snapshots built Linux amd64 and arm64 archives, DEBs, and RPMs.
Inspected session artifacts contain only the session executable and its
documentation/integration assets; DEB runtime metadata lists only Sway.

## Desktop application check

Use a disposable desktop entry and workspace 98 or higher. Never reuse or purge
an unrelated registration.

1. Focus one eligible normal top-level and preview register-focused.
2. Approve it explicitly; verify the exact identity, protected launch snapshot,
   stable context mark, and list/status output.
3. Exercise follow close grace, pin/unpin, archive/activate, rebind, reapprove,
   and exact-ID forget.
4. With two indistinguishable matching windows, verify presence is true but no
   anchor is guessed or moved.
5. Queue a missing desired app and verify launch intent is durable before
   process start; daemon restart must not duplicate it in one compositor
   session.
6. Confirm a user focus or move invalidates stale automatic work.

Treat Chrome, Slack, and similar application-internal restoration as app-owned.
Scratchpad restore remains deferred. Native Wayland parent/type limits in Sway
1.12 remain documented rather than guessed around.

## AppArmor and broker check

For agent-report changes, run the real Unix-socket regression tests in
internal/agentreport and the v1 compatibility tests in internal/codexreport.
Verify multiple agent kinds, non-UUID session tokens, rejected commands and
unknown fields, payload limits, peer credentials, unrelated pane ancestry,
socket replacement, and unchanged legacy replies. These tests use disposable
roots and a fake Herdr API; they do not prove provider-specific hook or resume
behavior in a live Herdr session.

Static policy validation is part of make verify:

~~~sh
sh scripts/check-apparmor-policy.sh
~~~

When the matching profile can be loaded safely, use only package-installed
root-owned binaries and invoke:

~~~sh
/usr/share/doc/sway-session/scripts/verify-codex-boundary.sh \
  CONTEXT_UUID PANE_ID CODEX_SESSION_UUID \
  HERDR_HISTORY STATE_FILE HERDR_SOCKET
~~~

The verifier requires /usr/bin/sway-session to be owned by the sway-session
package. It proves the narrow positive report path and negative history/state
access. A pathname-socket connect mediation gap is a failed or explicitly
unsupported boundary, never a passing result.

## Package build check

Before v0.1.0 exists, do not fetch or checksum a nonexistent tag archive.
Create a temporary source archive from the current worktree, copy PKGBUILD to a
temporary build directory, point its source at that local archive, replace SKIP
with the archive's real sha256, regenerate .SRCINFO, then run:

~~~sh
makepkg --verifysource
makepkg --cleanbuild --clean --noconfirm
pacman -Qlp sway-session-0.1.0-1-ARCH.pkg.tar.zst
~~~

Inspect that the package contains /usr/bin/sway-session, completions, the
license, README, plan, verification guide, standalone Sway template, Herdr and
sway-session config templates, packaged Codex hook, AppArmor profile, and live
verifier—all below /usr/share/doc/sway-session where appropriate. It must
contain no animator binary, animation/audio asset, parec metadata, optional
dependency metadata, or old documentation root.

For the package split, test a clean install and the ownership transition with
actual built packages in an isolated package-manager root. The old combined
package may own /usr/bin/sway-session. Either upgrade sway-title-animator to a
version that no longer owns that path before installing sway-session, or
install both verified replacement packages in one transaction. Never use
--overwrite to conceal ownership mistakes.

## Release gate

Before tagging:

- make verify passes;
- the current worktree has no generated binary or transient package output;
- a clean GoReleaser snapshot passes and its contents are inspected;
- the temporary local-tarball Arch source package builds and tests;
- title-indicator fixtures match across repositories;
- real Sway/Herdr evidence above is current;
- package ownership transition evidence is recorded;
- the code-review workflow has no unresolved actionable finding;
- CI configuration targets only sway-session;
- GitHub release/AUR secrets and permissions are verified without publishing;
  and
- the release commit is on main.

Only then create immutable tag v0.1.0. The AUR workflow computes the actual
GitHub source archive checksum, refuses SKIP, verifies and builds the package,
publishes exact metadata, and opens the metadata-sync PR. Do not move a release
tag or invent a checksum.
