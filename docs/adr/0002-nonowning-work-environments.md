---
status: accepted
---

# Keep named work environments nonowning

For the [LAB-180 design](../named-work-environments.md), choose stable named
collections of existing context references in the owner-only SQLite metadata
store. Contexts retain lifecycle, identity, launcher and placement authority;
membership/deletion cannot duplicate or mutate that authority. This supports
shared applications and consistent manager edits/backup without synchronizing
a second mutable configuration file or cloning contexts per environment.
Implementation was authorized on 2026-10-16 under LAB-375. It adds
the optional SQLite extension and consistent backup validation described in the
design. The user selected the safe first delivery that focuses existing work
and defers missing launches with a reason. A future missing-launch capability
is separate from the ownership decision recorded here.
