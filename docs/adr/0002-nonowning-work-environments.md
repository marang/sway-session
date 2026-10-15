---
status: proposed
---

# Keep named work environments nonowning

For the [LAB-180 design](../named-work-environments.md), choose stable named
collections of existing context references in the owner-only SQLite metadata
store. Contexts retain lifecycle, identity, launcher and placement authority;
membership/deletion cannot duplicate or mutate that authority. This supports
shared applications and consistent manager edits/backup without synchronizing
a second mutable configuration file or cloning contexts per environment.
Implementation and its dedicated migration remain paused; this proposal changes
no runtime schema or release scope.
