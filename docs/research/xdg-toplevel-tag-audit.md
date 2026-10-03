# LAB-93: native Wayland window identity audit

Checked: **2026-10-03**. Public primary sources only; source revisions below are the observations for this audit, not claims about
installed workstation packages. No live application restart or profile inspection was performed. Gate and fallback rules are recommendations, not guarantees
supplied by upstream.

**Decision: the generic durable per-window identity gate remains unmet.** Sway 1.12 and the checked development revision expose client
tags through IPC, but the protocol permits duplicate and changing tags. Checked Chromium/Electron code does not supply the missing tag
integration; actual native-client adoption found in Spectacle supplies a shared role string, not an individual window identity. These
are separate limitations. [Sway IPC][Sjson], [protocol][Ptag], [Chromium checked subtree][Cstable], [Electron checked tree][Estable],
[Spectacle][Kapp].

**Tracking recommendation:** keep LAB-93's reminder open until both IPC exposure and deterministic client restart reuse are established
for the supported scope. Record the compositor leg as available, rather than preserving an obsolete blanket “Sway has no tags” blocker.
No implementation of a staging protocol or application wrapper is proposed here. [Sway][Sjson], [protocol][Ptag].

## Version and source pins

“Development” below means the project's actual branch name, pinned at lookup. Commit abbreviations are expanded in the linked primary
source references.

| Component | Stable/released source checked | Development source checked |
| --- | --- | --- |
| wayland-protocols | 1.49, `ee78491a237e` [release XML][Prelease] | `main`, `aa62366fb800` [XML][Ptag] |
| Sway | 1.12, `88869399f421`, released 2026-05-25 [release][Srelease], [build][Sbuild] | `master`, `1652c54b73f6` [build][SmBuild] |
| wlroots | 0.20.2, `d783533489e1` [build][Wbuild] | `master`, `c5c57cd31658` [build][WmBuild] |
| Chromium/Chrome Linux | Chrome 154.0.8037.97, announced 2026-10-01; Chromium tag commit `b510e9d7cd3a` [announcement][Crelease], [source][Cstable] | Chromium `main`, `eb3a4bb42677` [source][Cmain] |
| Electron | v44.5.1, `19c601167d4a`, released 2026-09-30 [release][Erelease], [DEPS][Edeps] | `main`, `df79406cecfd` [DEPS][EmDeps] |
| Electron's Chromium dependency | 152.0.7977.130, `2c592105bbcd` [DEPS][Edeps], [source][CEstable] | 156.0.8078.3, `03a4bd2b9182` [DEPS][EmDeps], [source][CEmain] |
| KDE KWindowSystem | v6.30.0, `802629c63985` [Wayland implementation][Kstable] | `master`, `0115a781d8e1` [implementation][Kmain] |
| Spectacle | No release-version inference made | `master`, `312ad41ffb56` [client call][Kapp] |
| GTK | No released-version claim made | `main`, `b9cd9290d56b` [Wayland backend][Gmain] |

Sway 1.12's build requires wlroots `>=0.20.0,<0.21.0`; wlroots 0.20.0's own release notes list xdg-toplevel-tag-v1 support. These
source pins do not imply a tested pairing of installed binaries. [Sway build][Sbuild], [wlroots release][Wrelease].

## What the standard actually guarantees

- **Maturity:** xdg-toplevel-tag-v1 remains in `staging/`, interface version 1, in both wayland-protocols 1.49 and checked `main`. Its
  XML still contains the testing-phase disclaimer. Upstream governance encourages staging adoption; incompatible changes require a new
  major protocol version. This is an adopted staging extension, not a protocol in `stable/`. [release XML][Prelease], [main XML][Ptag],
  [phase policy][Ppolicy].
- **Purpose versus contract:** restart identification is the stated purpose. The concrete operation assigns a client-chosen string to
  an `xdg_toplevel`. Neither a compositor-issued durable ID nor a client save/reload algorithm is specified by these requests. Restart
  reuse therefore needs application evidence. [complete interface][Ptag].
- **Uniqueness:** tags need not be unique across applications; a client may use one tag for several windows. Conflict handling belongs
  to compositor policy. A role such as `settings` is explicitly suitable. A tag is consequently not a generic unique per-window key,
  even when paired with an application ID. [set_toplevel_tag, XML lines 48–66][Ptag].
- **Lifetime:** setting during initial commit is recommended, but a client may change the tag later. Tag description is a separate
  translated presentation string. Neither immutability nor exact restoration of client-internal window state follows from the
  interface. [requests, XML lines 48–81][Ptag].
- **Privacy:** tags may be displayed and are preferably human-readable. The complete interface has no confidentiality,
  anti-correlation, or authentication guarantee. An opaque string does not become private merely through opacity; persistent equality
  can itself expose a relationship. The latter is an audit inference from the exposed client string and IPC serialization. [XML][Ptag],
  [IPC serializer][Sjson].

Do not substitute `foreign_toplevel_identifier`: ext-foreign-toplevel-list-v1 requires a unique identifier for the mapped toplevel,
valid only while mapped, and forbids reuse after unmap. It identifies the live object across observers, not the logical window after
application restart. Sway exposes that separate field alongside `tag`. [identifier, XML lines 193–216][Pforeign], [IPC][Sjson].
Likewise, xdg-shell `app_id` identifies the application and groups its windows; it does not distinguish two windows of that
application. [set_app_id][Pshell].

## Support matrix

“Absent” means absent in the named checked files/subtrees, not every downstream build. “Unproven” means documentary evidence is
insufficient; no live run occurred.

| Surface/source | Tag transport or IPC exposure | Deterministic individual-window restart identity |
| --- | --- | --- |
| Sway 1.12 | Creates manager v1; tree window JSON contains `tag`, string or null. [server][Sserver], [serializer][Sjson] | No client uniqueness/reuse enforcement in checked setter. [setter][Sxdg] |
| Sway checked `master` | Same manager/setter; JSON exposes `tag`. [server][SmServer], [setter][SmXdg], [JSON][SmJson] | Same limitation. [setter][SmXdg], [spec][Ptag] |
| Sway window events, both pins | Event `container` uses the same recursive serializer, so emitted window snapshots carry the field. [stable][Sevent], [development][SmEvent] | No dedicated tag-change event in checked setter or documented event kinds. [setter][Sxdg], [development setter][SmXdg], [event docs][Sdocs], [development docs][SmDocs] |
| wlroots 0.20.2 / checked `master` | Manager v1 emits `set_tag` and `set_description` signals. [stable][Wtag], [development][WmTag] | These handlers do not generate or persist IDs, or enforce uniqueness. [stable handlers][Wtag], [development handlers][WmTag] |
| Chromium stable / checked `main` | No tag integration found in checked Wayland platform subtree and protocol build file. [stable subtree][Cstable], [main subtree][Cmain], [stable build][Cbuild], [main build][CmBuild] | Unavailable through the checked upstream tag path; Chrome binary behavior remains untested. [same source scopes][Cstable], [main][Cmain] |
| Electron stable / checked `main` | No tag integration found in its trees, including Chromium patches; same absence in both pinned Chromium Wayland subtrees. [stable tree][Estable], [main tree][Emain], [dependency stable][CEstable], [dependency main][CEmain] | No demonstrated tag-based durable BrowserWindow identity. [stable API][Eapi], [main API][EmApi] |
| Slack Linux | Release notes identify 4.52.171 on 2026-09-29 and discuss Wayland fixes in 4.51.180; they do not document tag emission/reuse. [first-party notes][Slack] | **Unproven**, not “unsupported because Electron version X.” No Slack binary, bundled runtime, or private profile inspected. [checked notes][Slack] |
| Native Spectacle + KWindowSystem | Actual `setXdgToplevelTag(this, "region-editor")` call; KWindowSystem sends the protocol request on a Wayland toplevel. [app][Kapp], [library][Kmain] | Constant role string shared by CaptureWindow instances; no individual-window key in this call. [constructor and instance list][Kapp] |
| GTK checked `main` | No tag protocol symbols/calls found in `gdk/wayland/`. [backend][Gmain] | No automatic tag identity established by this source audit; application-specific clients were not exhaustively audited. [backend scope][Gmain] |
| XWayland under checked Sway | No tag path: its property switch returns null for `VIEW_PROP_TAG`; JSON therefore emits null. [XWayland switch][Sxway], [view accessor][Sview], [JSON][Sjson] | Requires conservative existing fallback; X11 `window_role` is a different property, not an xdg tag. [switch][Sxway], [IPC docs][Sdocs] |

## Source tracing and bounded absence checks

Sway: `sway/server.c` creates the tag manager and installs only the tag listener;
`sway/desktop/xdg_shell.c::xdg_toplevel_tag_manager_v1_handle_set_tag` copies the string, executes criteria, and commits dirty
transactions. It does not directly emit an IPC window event or maintain a restart database. `sway/ipc-json.c` serializes
`view_get_tag`; `sway/ipc-server.c::ipc_event_window` reuses recursive node serialization. A later unrelated event may carry a changed
tag, but an observer must not require such an event to occur. [server][Sserver], [setter][Sxdg], [JSON][Sjson], [events][Sevent];
[development equivalents][SmXdg], [JSON][SmJson].

Chromium: searched complete `ui/ozone/platform/wayland/` archives at all four Chromium pins above (stable/main: 385/389 files; Electron
dependency pins: 384/389 files) plus `third_party/wayland-protocols/BUILD.gn` at each pin. The expression
`xdg.?toplevel.?tag|set_toplevel_tag|[Tt]oplevel[Tt]ag` returned no matches. Inspected registry/window implementation and
`host/xdg_toplevel.cc`; the latter calls `xdg_toplevel_set_app_id`, not the tag protocol. This bounds the negative result to those
source paths; it is not a complete Chromium-tree or proprietary Chrome audit. [stable archive][Ca], [main archive][Cma], [dependency
archives][CEa], [development dependency archive][CEma], [protocol build][Cbuild], [main build][CmBuild], [app-id call][Cxdg].

Electron: searched complete pinned trees (3,262 stable / 3,107 development regular files), including `shell/`, `patches/chromium/`, and
`docs/api/`, with the same expression and found no matches. Examined BrowserWindow/BaseWindow API and native-window code; no documented
durable xdg tag contract was established. This covers these source revisions, not private downstream patches or Slack's closed
application behavior. [stable archive][Ea], [main archive][Ema], [stable API][Eapi], [main API][EmApi], [native window][Enative].

Native adoption: KWindowSystem's public API documents availability since 6.22; the checked v6.30.0 and development implementations
actually send the request and repeat it when a surface role is recreated. That repeats the supplied value inside the client process; it
does not persist/reconstruct an application's logical windows across process restart. Spectacle's introduction commit and current call
demonstrate adoption independent of a guessed toolkit version. [API][Kapi], [released implementation][Kstable], [development
implementation][Kmain], [introduction commit][Kintro], [current call][Kapp].

## Exact implementation gate and conservative fallback

Require **all** of the following before enabling generic automatic per-window restore; these are proposed acceptance conditions
motivated by the sources above:

1. **Observable identity:** the target Sway IPC tree exposes the actual client tag for the exact mapped window, including
   missing/null/empty and updates. Use fresh bounded observations, including after reconnect and before effects; account for the
   absence of a guaranteed tag-change event. [IPC][Sjson], [setter][Sxdg].
2. **Deterministic restart reuse:** an application contract and disposable-state restart evidence demonstrate that each logical window
   saves/reuses its own ID, distinguishes siblings, and never silently assigns an old ID to a different logical window. Include normal
   restart, relevant crash recovery, and multiple instances. Protocol purpose alone does not satisfy this condition. [spec][Ptag].
3. **Unambiguous namespace:** combine a validated existing application/launcher association with exact tag equality; require one
   durable owner and one live candidate throughout the relevant scope. A tag alone is neither globally unique nor authenticated.
   [spec][Ptag].
4. **Honest degradation:** conflicts or missing evidence retire automatic exact restore eligibility and retain recoverable saved data.
   Do not manufacture an ID from title, focus/creation order, URL, command line, PID, or a role label. The client-tag contract supplies
   no such reconstruction algorithm. [spec][Ptag].

Current assessment: condition 1 has upstream source support on checked Sway; condition 2 has no qualifying evidence for the generic
Chromium/Electron/Slack scope, and a Spectacle role tag does not satisfy it. Conditions 3–4 remain design and validation obligations.
This is an implementation gate failure, not proof that no application anywhere can expose durable IDs. [IPC][Sjson],
[Chromium][Cstable], [Electron][Estable], [Slack evidence limit][Slack], [Spectacle][Kapp], [spec][Ptag].

| Observation | Proposed behavior |
| --- | --- |
| Field absent, null, empty, malformed, or beyond a chosen input bound | Treat per-window identity as unavailable; a bounded settling wait may obtain fresh evidence, then degrade. Never synthesize a replacement. |
| Tag changes on a live bound window | Invalidate pending exact restore for that association; preserve the old durable record and require explicit rebind/reapproval. |
| Restarted client supplies a different tag | Treat the former window as unresolved; do not transfer its layout by title/order/URL/command line. |
| Two live windows share application namespace + tag | Mark the entire candidate group ambiguous; select neither and apply no exact-window effect. |
| Two saved owners claim one key, or a key collides with another validated association | Preserve both records and refuse automatic adoption/overwrite; require explicit resolution. |
| A duplicate later disappears | Fresh uniqueness alone does not prove which logical window survived; require the established restart contract or explicit resolution. |
| Missing support or XWayland | Retain only independently authorized application-level presence/placement behavior and explicit focused-window binding; never infer individual restore identity. |

Privacy recommendation: treat even opaque tags as potentially sensitive, client-controlled correlation values. If future identity
capture is authorized, keep it in owner-only durable state, bound inputs, avoid raw tags in routine logs and presentation marks, and do
not derive tags by reading private profiles, URLs, or command lines. Hashing a stable string is not evidence of unlinkability. This
recommendation follows from the protocol's displayable string semantics and Sway's raw IPC exposure, not an upstream privacy promise.
[spec][Ptag], [IPC][Sjson].

## Unresolved evidence and later follow-up

- Obtain an explicit deterministic client-window persistence contract before choosing any target application; confirm supported
  release/build and exact restore behavior using disposable state, without user profiles or secret access.
- Verify actual `get_tree` and window-event payloads, delayed tag assignment, changes, reconnect, duplicates, and restart reuse in
  isolated compositor tests on workspace 98 or higher. None of those live checks ran in this audit.
- Slack's notes establish Wayland-related maintenance only. Tag support, bundled implementation, and restart behavior remain unknown.
  [checked first-party notes][Slack].
- The requested [Wayland Explorer page](https://wayland.app/protocols/xdg-toplevel-tag-v1) was consulted as a discovery page; its
  compositor table was not used as primary support evidence. Freedesktop HTML retrieval failed/was challenged in places; authoritative
  XML and source were retrieved through unauthenticated public APIs; [pinned XML API fallback][Praw].
- Negative findings are bounded to the pinned paths above. They exclude installed binaries, proprietary patches, all other native apps,
  and any later revisions. Preserve this date/pin boundary when integrating; keep LAB-93 open if the gate remains unmet. This file is
  research only: no wrappers or protocol implementation.

## Primary source references

[Ptag]: https://gitlab.freedesktop.org/wayland/wayland-protocols/-/blob/aa62366fb800a0689f4e9de83811ff33f6a91f44/staging/xdg-toplevel-tag/xdg-toplevel-tag-v1.xml
[Praw]: https://gitlab.freedesktop.org/api/v4/projects/wayland%2Fwayland-protocols/repository/files/staging%2Fxdg-toplevel-tag%2Fxdg-toplevel-tag-v1.xml/raw?ref=aa62366fb800a0689f4e9de83811ff33f6a91f44
[Prelease]: https://gitlab.freedesktop.org/wayland/wayland-protocols/-/blob/ee78491a237eaff9389a0ccf8680521d074407d3/staging/xdg-toplevel-tag/xdg-toplevel-tag-v1.xml
[Ppolicy]: https://gitlab.freedesktop.org/wayland/wayland-protocols/-/blob/aa62366fb800a0689f4e9de83811ff33f6a91f44/README.md
[Pforeign]: https://gitlab.freedesktop.org/wayland/wayland-protocols/-/blob/aa62366fb800a0689f4e9de83811ff33f6a91f44/staging/ext-foreign-toplevel-list/ext-foreign-toplevel-list-v1.xml
[Pshell]: https://gitlab.freedesktop.org/wayland/wayland-protocols/-/blob/aa62366fb800a0689f4e9de83811ff33f6a91f44/stable/xdg-shell/xdg-shell.xml
[Srelease]: https://github.com/swaywm/sway/releases/tag/1.12
[Sbuild]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/meson.build#L42
[Sserver]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/server.c#L605
[Sxdg]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/desktop/xdg_shell.c#L595
[Sjson]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/ipc-json.c#L630
[Sevent]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/ipc-server.c#L322
[Sdocs]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/sway-ipc.7.scd#L1643
[Sxway]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/desktop/xwayland.c#L217
[Sview]: https://github.com/swaywm/sway/blob/88869399f421d9180dd8b6ed0b5a1f4a3585d252/sway/tree/view.c#L242
[SmBuild]: https://github.com/swaywm/sway/blob/1652c54b73f67df17b7b4ab0b0f7048204aa8104/meson.build
[SmServer]: https://github.com/swaywm/sway/blob/1652c54b73f67df17b7b4ab0b0f7048204aa8104/sway/server.c
[SmXdg]: https://github.com/swaywm/sway/blob/1652c54b73f67df17b7b4ab0b0f7048204aa8104/sway/desktop/xdg_shell.c#L595
[SmJson]: https://github.com/swaywm/sway/blob/1652c54b73f67df17b7b4ab0b0f7048204aa8104/sway/ipc-json.c
[SmEvent]: https://github.com/swaywm/sway/blob/1652c54b73f67df17b7b4ab0b0f7048204aa8104/sway/ipc-server.c
[SmDocs]: https://github.com/swaywm/sway/blob/1652c54b73f67df17b7b4ab0b0f7048204aa8104/sway/sway-ipc.7.scd
[Wbuild]: https://gitlab.freedesktop.org/wlroots/wlroots/-/blob/d783533489e1f75d6886c2ab5c5960090ef268f8/meson.build
[WmBuild]: https://gitlab.freedesktop.org/wlroots/wlroots/-/blob/c5c57cd316581c0779a51309f1ae90b7e1abd8c4/meson.build
[Wrelease]: https://gitlab.freedesktop.org/wlroots/wlroots/-/releases/0.20.0
[Wtag]: https://gitlab.freedesktop.org/wlroots/wlroots/-/blob/d783533489e1f75d6886c2ab5c5960090ef268f8/types/wlr_xdg_toplevel_tag_v1.c
[WmTag]: https://gitlab.freedesktop.org/wlroots/wlroots/-/blob/c5c57cd316581c0779a51309f1ae90b7e1abd8c4/types/wlr_xdg_toplevel_tag_v1.c
[Crelease]: https://chromereleases.googleblog.com/2026/10/stable-channel-update-for-desktop.html
[Cstable]: https://chromium.googlesource.com/chromium/src/+/b510e9d7cd3a2fbd78d0ddc42234103206c5f78d/ui/ozone/platform/wayland/
[Cmain]: https://chromium.googlesource.com/chromium/src/+/eb3a4bb42677ae222f49978904037b64093dc000/ui/ozone/platform/wayland/
[CEstable]: https://chromium.googlesource.com/chromium/src/+/2c592105bbcd9490a9894df48d0fe59b2c512651/ui/ozone/platform/wayland/
[CEmain]: https://chromium.googlesource.com/chromium/src/+/03a4bd2b9182691ca7d80e876878f678029aef83/ui/ozone/platform/wayland/
[Ca]: https://chromium.googlesource.com/chromium/src/+archive/b510e9d7cd3a2fbd78d0ddc42234103206c5f78d/ui/ozone/platform/wayland.tar.gz
[Cma]: https://chromium.googlesource.com/chromium/src/+archive/eb3a4bb42677ae222f49978904037b64093dc000/ui/ozone/platform/wayland.tar.gz
[CEa]: https://chromium.googlesource.com/chromium/src/+archive/2c592105bbcd9490a9894df48d0fe59b2c512651/ui/ozone/platform/wayland.tar.gz
[CEma]: https://chromium.googlesource.com/chromium/src/+archive/03a4bd2b9182691ca7d80e876878f678029aef83/ui/ozone/platform/wayland.tar.gz
[Cbuild]: https://chromium.googlesource.com/chromium/src/+/b510e9d7cd3a2fbd78d0ddc42234103206c5f78d/third_party/wayland-protocols/BUILD.gn
[CmBuild]: https://chromium.googlesource.com/chromium/src/+/eb3a4bb42677ae222f49978904037b64093dc000/third_party/wayland-protocols/BUILD.gn
[Cxdg]: https://chromium.googlesource.com/chromium/src/+/b510e9d7cd3a2fbd78d0ddc42234103206c5f78d/ui/ozone/platform/wayland/host/xdg_toplevel.cc#204
[Erelease]: https://github.com/electron/electron/releases/tag/v44.5.1
[Edeps]: https://github.com/electron/electron/blob/19c601167d4ae75989938416c18ed81eb21c6020/DEPS#L4
[EmDeps]: https://github.com/electron/electron/blob/df79406cecfd393f38d5a18b83379d4ba85cc29b/DEPS#L4
[Estable]: https://github.com/electron/electron/tree/19c601167d4ae75989938416c18ed81eb21c6020
[Emain]: https://github.com/electron/electron/tree/df79406cecfd393f38d5a18b83379d4ba85cc29b
[Ea]: https://codeload.github.com/electron/electron/tar.gz/19c601167d4ae75989938416c18ed81eb21c6020
[Ema]: https://codeload.github.com/electron/electron/tar.gz/df79406cecfd393f38d5a18b83379d4ba85cc29b
[Eapi]: https://github.com/electron/electron/blob/19c601167d4ae75989938416c18ed81eb21c6020/docs/api/browser-window.md
[EmApi]: https://github.com/electron/electron/blob/df79406cecfd393f38d5a18b83379d4ba85cc29b/docs/api/browser-window.md
[Enative]: https://github.com/electron/electron/blob/19c601167d4ae75989938416c18ed81eb21c6020/shell/browser/native_window_views.cc
[Slack]: https://slack.com/release-notes/linux
[Kstable]: https://github.com/KDE/kwindowsystem/blob/802629c63985e29f8288256b225e97419423c7a6/src/platforms/wayland/windowsystem.cpp#L336
[Kmain]: https://github.com/KDE/kwindowsystem/blob/0115a781d8e1008a2136823801fd9853ed23f930/src/platforms/wayland/windowsystem.cpp#L336
[Kapi]: https://github.com/KDE/kwindowsystem/blob/0115a781d8e1008a2136823801fd9853ed23f930/src/kwaylandextras.h#L118
[Kintro]: https://github.com/KDE/spectacle/commit/91d26010dffe2a4019fdfe56066ebd3765bd1fc9
[Kapp]: https://github.com/KDE/spectacle/blob/312ad41ffb566bbb2169b5120546ad21942d924e/src/Gui/CaptureWindow.cpp#L50
[Gmain]: https://github.com/GNOME/gtk/tree/b9cd9290d56ba0730c2bc726502fbad0e2288b8c/gdk/wayland
