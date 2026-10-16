# Explicit remote terminal bindings

Status: accepted for LAB-376, authorized 2026-10-16.

A remote terminal context pins an SSH target and remote Herdr session separately
from its local context/session identity and local launch directory. Saved-machine
bindings also pin the profile ID; every launch checks that the enabled profile
still names the exact target/session. Both origins use Herdr's direct attach,
which avoids shared machine selection. Labels never identify remote work.

The optional binding is inside the strict context payload. Existing local
payloads and schema versions remain unchanged. Older binaries reject a remote
payload's unknown field rather than interpret it as a local launcher; downgrade
requires removing remote registrations with the newer binary first. Backup,
lifecycle and restore fingerprints include the binding through that payload.

Herdr owns transport, remote server startup and interactive setup. No credentials,
SSH command templates, per-launch global selection changes or parallel reconnect
loop are added. Direct attach has no public no-install flag in Herdr0.9.5:
installation/update remains behind Herdr's own explicit confirmation; we never
answer it. A capability/configuration change after validation can still produce
a visible Herdr prompt. A mapped window does not prove remote readiness.

Remote contexts never use local Herdr socket/directory observations as remote
health, initialize remote panes, accept local agent reports or purge remote work.
Remote window absence cannot distinguish failed transport from deliberate close.
It therefore never automatically archives a remote context; explicit archive
disables restore eligibility and forget removes registration. No new automatic
retry loop is introduced. Local terminal close behavior stays unchanged.
