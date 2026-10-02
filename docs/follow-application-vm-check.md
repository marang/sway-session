# Follow application persistence across a real VM reboot (LAB-141)

This is an **opt-in, unperformed procedure** for a normal guest reboot. Injected
guard tests and private-compositor close/exit tests provide separate evidence;
they do not establish persistence across a real reboot. LAB-141 protects Follow
close decisions with the existing logind monitor and Sway event-stream guard.
An unsafe generation discards close evidence without clearing adoption/launch
bookkeeping; a fresh tree and an in-memory guard recheck precede writing
`desired_open=false`. This procedure does not
validate LAB-140 launch freshness or introduce an API/schema change.

## Prerequisites and boundary

Use an **existing disposable Linux VM**, a disposable guest account, and a
recoverable VM snapshot. Its guest disk must survive the reboot. Do not revert
the snapshot between the before/after reads. No VM image download or installation
is part of this check. If prerequisites are absent, record **not run**.

The guest must already have Sway, working systemd-logind/system D-Bus, Bash,
Alacritty at `/usr/bin/alacritty`, `/usr/bin/sleep`, jq, SQLite with JSON support,
`systemd-detect-virt`, `systemd-inhibit`, and permission to reboot normally.
Stage the candidate binary containing the LAB-141 fix at
`$HOME/lab141-candidate/sway-session` **inside the guest**. Record its build/commit.
Use a guest Sway login with no sway-session autostart; manually start only the
test daemon below. Keep a control terminal on workspace 99.

Run every following command in that guest console. Confirm its VM identity in
the hypervisor UI as well as with `systemd-detect-virt --vm`. Never run host
shutdown, reboot, suspend, service changes, or commands against live user state.
Do not mount a real home/state directory into the guest. All test window creation,
focus, close, and restore must stay on workspace 98 or higher. A container,
private compositor alone, or a forced hypervisor reset is insufficient.

## Isolated setup and healthy-close registration

In the guest control terminal, start Bash. Stop on unexpected command failures.
The test directory must not already exist:

```sh
bash
set -euo pipefail
systemd-detect-virt --vm
test ! -e "$HOME/lab141-follow-check"
umask 077
mkdir "$HOME/lab141-follow-check"
cat > "$HOME/lab141-follow-check/env.sh" <<'EOF'
systemd-detect-virt --vm >/dev/null || return 1
umask 077
export LAB141_ROOT="$HOME/lab141-follow-check"
export LAB141_BIN="$HOME/lab141-candidate/sway-session"
export LAB141_LOGIN_RUNTIME="${LAB141_LOGIN_RUNTIME:-${XDG_RUNTIME_DIR:?}}"
case "${WAYLAND_DISPLAY:?}" in
  /*) ;;
  *) export WAYLAND_DISPLAY="$LAB141_LOGIN_RUNTIME/$WAYLAND_DISPLAY" ;;
esac
export XDG_STATE_HOME="$LAB141_ROOT/state"
export XDG_CONFIG_HOME="$LAB141_ROOT/config"
export XDG_DATA_HOME="$LAB141_ROOT/data"
export XDG_RUNTIME_DIR="$LAB141_LOGIN_RUNTIME/lab141-follow-check"
export WINIT_UNIX_BACKEND=wayland
LAB141_DB="$XDG_STATE_HOME/sway-session/state.sqlite3"
test -x "$LAB141_BIN"
test -S "${SWAYSOCK:?}"
install -d -m 700 "$XDG_STATE_HOME" "$XDG_CONFIG_HOME" \
  "$XDG_DATA_HOME/applications" "$XDG_RUNTIME_DIR"

lab141_window() {
  swaymsg -s "$SWAYSOCK" -t get_tree | jq -er --arg app "${1:-lab141-follow}" '
    [.. | objects | select(.app_id? == $app)] as $all |
    [.. | objects | select(.type? == "workspace" and .num >= 98) |
      .. | objects | select(.app_id? == $app)] as $high |
    if ($all | length) == 1 and ($high | length) == 1
    then $all[0].id else error("need exactly one test window on workspace 98+") end'
}
lab141_launch() {
  swaymsg -s "$SWAYSOCK" 'workspace number 98'
  /usr/bin/alacritty --class "${1:-lab141-follow}" -e /usr/bin/sleep infinity \
    > "$LAB141_ROOT/${1:-lab141-follow}.log" 2>&1 &
}
lab141_close() {
  local lab141_con
  lab141_con=$(lab141_window "${1:-lab141-follow}")
  swaymsg -s "$SWAYSOCK" "[con_id=$lab141_con] focus"
  swaymsg -s "$SWAYSOCK" "[con_id=$lab141_con] kill"
}
lab141_evidence() {
  local lab141_id
  lab141_id=$(cat "${1:-$LAB141_ROOT/context-id}")
  [[ "$lab141_id" =~ ^[0-9a-f-]{36}$ ]] || return 1
  sqlite3 -readonly -header -separator $'\t' "$LAB141_DB" "
    SELECT id,
      json_extract(CAST(payload AS TEXT), '$.state') AS state,
      json_extract(CAST(payload AS TEXT), '$.app.restore_policy') AS restore_policy,
      json_extract(CAST(payload AS TEXT), '$.app.desired_open') AS desired_open
    FROM contexts WHERE id = '$lab141_id';"
}
EOF
source "$HOME/lab141-follow-check/env.sh"
"$LAB141_BIN" --json version > "$LAB141_ROOT/build.json"
for lab141_app in lab141-close-control lab141-follow; do
cat > "$XDG_DATA_HOME/applications/$lab141_app.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=LAB-141 disposable $lab141_app
Exec=/usr/bin/alacritty --class $lab141_app -e /usr/bin/sleep infinity
StartupWMClass=$lab141_app
Terminal=false
EOF
done
lab141_launch lab141-close-control
```

Wait for the blank test window to map, then obtain its exact container ID and
focus it from the control shell immediately before registration. The unique
Wayland app ID matches the disposable desktop entry. This ordinary application
window runs only `sleep`; it is not a registered managed terminal.
The healthy-close control uses `lab141-close-control` and its own UUID. The
reboot application will be registered separately as `lab141-follow`; never
close it in a healthy pre-reboot control or reset its intent to prepare reboot.

```sh
LAB141_WINDOW=$(lab141_window lab141-close-control)
swaymsg -s "$SWAYSOCK" "[con_id=$LAB141_WINDOW] focus"
"$LAB141_BIN" --json app register-focused --socket "$SWAYSOCK" \
  --desktop-id lab141-close-control.desktop --yes |
  jq -er '.contexts | if length == 1 then .[0].id else error("unexpected registration") end' \
  > "$LAB141_ROOT/control-context-id"
lab141_evidence "$LAB141_ROOT/control-context-id" | tee "$LAB141_ROOT/control-registered.tsv"
nohup "$LAB141_BIN" daemon --socket "$SWAYSOCK" \
  > "$LAB141_ROOT/daemon-before.log" 2>&1 &
LAB141_DAEMON_PID=$!
sleep 10
kill -0 "$LAB141_DAEMON_PID"
"$LAB141_BIN" --json doctor --check --socket "$SWAYSOCK" \
  > "$LAB141_ROOT/doctor-before.json" || printf '%s\n' 'Inspect doctor failures locally.'
jq '.doctor.checks[] | {id, status}' "$LAB141_ROOT/doctor-before.json"
systemd-inhibit --list --no-pager
```

Require one SQLite row: the saved UUID, `active`, `follow`, `1` (true). Inspect
doctor's Sway IPC, private paths, daemon lock, and binary evidence; it must refer
to the candidate daemon. Missing optional Herdr/terminal setup or Sway autostart
integration can be recorded for this manually started application check. Do not
apply doctor repairs. Doctor does not certify shutdown protection: additionally
require the daemon's exact PID to hold a `shutdown:sleep` **delay** inhibitor and
no unavailable/disabled close-detection diagnostic in `daemon-before.log`.

## Healthy user-close control and reboot baseline

While logind and the daemon remain healthy, issue a normal Sway close against
the exact test window. Refresh its ID immediately before closing; the helper
refuses ambiguous or low-workspace targets. Do not close the control terminal.

```sh
lab141_close lab141-close-control
sleep 10
kill -0 "$LAB141_DAEMON_PID"
lab141_evidence "$LAB141_ROOT/control-context-id" | tee "$LAB141_ROOT/healthy-close.tsv"
```

After the close grace (currently two seconds; ten seconds allows reconciliation),
require the same UUID, `active`, `follow`, `0`. If it remains true, stop: a
disabled/unhealthy guard must not masquerade as a positive shutdown result.
Leave that control registration false. Now launch and register the separate
reboot application with a **new UUID**; its intent starts true and must stay true
throughout the pre-reboot phase. No pin/unpin, activate, or database reset is used:

```sh
lab141_launch
```

After mapping:

```sh
LAB141_WINDOW=$(lab141_window)
swaymsg -s "$SWAYSOCK" "[con_id=$LAB141_WINDOW] focus"
"$LAB141_BIN" --json app register-focused --socket "$SWAYSOCK" \
  --desktop-id lab141-follow.desktop --yes |
  jq -er '.contexts | if length == 1 then .[0].id else error("unexpected registration") end' \
  > "$LAB141_ROOT/context-id"
test "$(cat "$LAB141_ROOT/context-id")" != "$(cat "$LAB141_ROOT/control-context-id")"
sleep 10
lab141_evidence | tee "$LAB141_ROOT/before-reboot.tsv"
cat /proc/sys/kernel/random/boot_id > "$LAB141_ROOT/before-boot-id"
kill -0 "$LAB141_DAEMON_PID"
systemd-inhibit --list --no-pager
```

Require the new reboot UUID, `active`, `follow`, `1`, the exact delay inhibitor,
and a healthy daemon log. Leave both the app and daemon running. **Only in the verified
disposable guest console**, request a normal guest reboot:

```sh
systemd-detect-virt --vm
systemctl reboot
```

Use guest privilege elevation if required by its existing policy. Do not kill
the app/daemon first, exit Sway manually, or substitute a host power action.

## After reboot: durable evidence, then automatic restore

Log back into the same guest account and Sway. Open the control terminal on
workspace 99. Do not start the candidate daemon or launch the test app yet.
The guest must not have autostarted either. In a new Bash shell:

```sh
set -euo pipefail
source "$HOME/lab141-follow-check/env.sh"
cat /proc/sys/kernel/random/boot_id > "$LAB141_ROOT/after-boot-id"
! cmp -s "$LAB141_ROOT/before-boot-id" "$LAB141_ROOT/after-boot-id"
lab141_evidence | tee "$LAB141_ROOT/after-reboot-before-daemon.tsv"
```

Require a changed boot ID and the **same UUID**, `active`, `follow`, `1` in the
original guest SQLite database. A missing row, false intent, or query error is
a failure; report it before allowing a new presence observation to change state.
Do not dump SQLite, copy its live files, or read unrelated rows. The query casts
the JSON payload BLOB to text and exposes only these four test fields.

Now start the candidate daemon with the same roots and the current Sway socket:

```sh
swaymsg -s "$SWAYSOCK" 'workspace number 98'
nohup "$LAB141_BIN" daemon --socket "$SWAYSOCK" \
  > "$LAB141_ROOT/daemon-after.log" 2>&1 &
LAB141_DAEMON_PID=$!
sleep 10
kill -0 "$LAB141_DAEMON_PID"
LAB141_WINDOW=$(lab141_window)
swaymsg -s "$SWAYSOCK" "[con_id=$LAB141_WINDOW] focus"
"$LAB141_BIN" --json app status --socket "$SWAYSOCK" |
  jq '.contexts[] | {id, state, restore_policy: .app.restore_policy, desired_open: .app.desired_open}'
lab141_evidence | tee "$LAB141_ROOT/after-restore.tsv"
systemd-inhibit --list --no-pager
```

Wait up to 30 seconds for mapping if necessary, refreshing `LAB141_WINDOW`.
Require one automatically restored test window on workspace 98, its existing
UUID in focused `app status`, and true intent. `app status` takes no context
argument. Do not manually launch the app to rescue this assertion. Inspect the
post-reboot log/inhibitor. Leave the restored reboot application open; its
registration has never been reset or closed while healthy. The separate
healthy-close control remains false and must not restore automatically.

## Monitor-unavailable control and explicit archive

With the restored reboot window present and true intent recorded, stop only the
test daemon. Restart it with an unavailable system bus address scoped to this
one process; leave guest logind, the system bus, and Sway running:

```sh
kill -TERM "$LAB141_DAEMON_PID"
wait "$LAB141_DAEMON_PID" || true
test ! -e "$LAB141_ROOT/no-system-bus"
DBUS_SYSTEM_BUS_ADDRESS="unix:path=$LAB141_ROOT/no-system-bus" \
  nohup "$LAB141_BIN" daemon --socket "$SWAYSOCK" \
  > "$LAB141_ROOT/daemon-no-monitor.log" 2>&1 &
LAB141_DAEMON_PID=$!
sleep 10
kill -0 "$LAB141_DAEMON_PID"
lab141_evidence
sed -n '1,40p' "$LAB141_ROOT/daemon-no-monitor.log"
```

Require the unavailable automatic-close diagnostic before closing the app:

```sh
lab141_close
for lab141_sample in {1..10}; do
  sleep 1
  lab141_evidence
done | tee "$LAB141_ROOT/no-monitor-close.tsv"
```

After closing, every sampled row must retain
`active`, `follow`, `1`; missing logind cannot prove a user close. A later
ordinary restore attempt does not change that assertion. Then check explicit
archive and restart with the ordinary bus environment:

```sh
"$LAB141_BIN" app archive "$(cat "$LAB141_ROOT/context-id")"
lab141_evidence | tee "$LAB141_ROOT/explicit-archive.tsv"
kill -TERM "$LAB141_DAEMON_PID"
wait "$LAB141_DAEMON_PID" || true
LAB141_COUNT=$(swaymsg -s "$SWAYSOCK" -t get_tree |
  jq '[.. | objects | select(.app_id? == "lab141-follow")] | length')
if [ "$LAB141_COUNT" -eq 1 ]; then lab141_close; else test "$LAB141_COUNT" -eq 0; fi
swaymsg -s "$SWAYSOCK" 'workspace number 98'
nohup "$LAB141_BIN" daemon --socket "$SWAYSOCK" \
  > "$LAB141_ROOT/daemon-archived.log" 2>&1 &
LAB141_DAEMON_PID=$!
sleep 10
kill -0 "$LAB141_DAEMON_PID"
swaymsg -s "$SWAYSOCK" -t get_tree |
  jq -e '[.. | objects | select(.app_id? == "lab141-follow")] | length == 0'
lab141_evidence
```

Require `archived`, `follow`, `1` immediately after archive and after restart,
with no automatic restoration. Archive changes eligibility, not this intent bit.

## Pass criteria and record

Pass requires all three independent observations: healthy user closes record
false; the normal VM reboot retains true **before daemon startup** and restores
the same Follow registration; unavailable monitoring preserves true while
explicit archive remains authoritative. The reboot must use the real guest
logind path, with the app and daemon alive at the request. These observations
complement injected race/generation and private-compositor tests; they do not
prove every possible shutdown ordering or forced power-loss behavior.

Record the candidate commit/build, guest VM/software versions, changed guest
boot IDs, narrow TSV rows, and pass/fail/not-run status for each control. Share
only those test fields and redacted relevant diagnostics, never database files,
full Sway trees, environment dumps, pane history, captured terminal contents,
or launcher snapshots.
Keep artifacts private in the guest and outside Git. After collecting the
record, stop the exact test daemon and discard the disposable guest/snapshot.
No execution result is claimed by this document.
