# Sonarr integration

Marauder can **automatically take over monitoring** of updateable forum-tracker
topics that Sonarr grabs. This closes the one gap in the *arr workflow for
forum trackers like RuTracker, Kinozal, and NNM-Club: a "release" there is an
**updateable forum topic** whose `.torrent`/infohash changes over time as new
episodes are added, and Sonarr — built around the Torznab "one release =
one immutable file" model — can grab the initial release but can't keep
watching that topic for future updates.

With the integration enabled, the ownership model is clean:

- **Seerr / Sonarr** stay the entry point and do the initial grab.
- **Marauder** automatically takes over monitoring of the grabbed forum topic
  and pushes future torrent updates to the **same** download client and
  category Sonarr imports from — so Sonarr keeps importing completed files with
  no category/path drift.

It is configured at **Integrations → Sonarr** in the Marauder UI (admin only).

## How it works

1. A background poller reads Sonarr's grab history
   (`GET /api/v3/history/since?eventType=grabbed`) every poll interval.
2. For each grab, it reads the tracker **topic URL** from `data.nzbInfoUrl`
   (the info/details page — *not* `guid`, which for some trackers is a download
   link).
3. If the URL matches an installed Marauder tracker plugin (and is in the
   allowed-trackers list, if you set one), Marauder auto-creates a monitored
   **topic** owned by the admin, using the configured default client / category
   / download directory. The grabbed release title and torrent infohash are
   retained as tracker metadata so plugins with multiple torrent variants can
   keep monitoring the same quality/codec Sonarr selected. A later grab for the
   same Sonarr-created topic refreshes this variant metadata even when
   **Update existing topics** is disabled; that setting controls only
   client/category/path realignment. Manually-created topics are never pinned
   to a Sonarr grab as a side effect.
4. From then on Marauder's scheduler monitors that topic for hash changes and
   delivers updated torrents — exactly like a manually-added topic. Auto-created
   topics carry a **Sonarr** badge in the Topics list.

The poll is deliberately conservative:

- **Go-forward on first enable** — enabling the integration stamps the cursor
  to "now" and imports **nothing** historical, so you don't get a topic per
  past grab. New grabs flow from that point on.
- **Idempotent** — deduped by topic URL (a season-pack grab that fans out into
  N episode-history rows still creates exactly one topic), with a
  unique-constraint backstop.
- **Fail-open** — if Sonarr is unreachable the poll logs a warning and retries
  next tick; the cursor never advances past unprocessed grabs.

## Multiple instances

Marauder supports **any number of Sonarr instances** — e.g. one Sonarr for
regular TV and a separate one for anime, each with its own download client,
category, save path, allowed-tracker set, and **independent history cursor**.
Each instance polls on its own interval and is enabled/disabled independently.

In the Marauder UI, open **Integrations → Sonarr** (admin only). You'll see a
card per configured instance with its status (**Active**, **Paused**, or
**Draft**), and an **Add instance** button. Each card lets you enable/disable,
test, edit, or delete that instance.

A new instance defaults to **disabled** ("Draft") so you can fill it in and
**Test connection** before it starts polling — flip **Enable** when ready.

## Configuration

Each instance has these fields:

| Field | What it does |
|---|---|
| **Name** | A label to tell instances apart (e.g. `TV`, `Anime`). |
| **Enable integration** | Turns this instance's poller on. |
| **Sonarr URL** | This Sonarr's base URL, reachable from the Marauder backend (e.g. `http://sonarr:8989` on the same Docker network). |
| **API key** | Sonarr → Settings → General → API Key. Stored encrypted at rest; never returned by the API (the UI shows only whether one is set). Leave blank when editing other fields to keep the stored key. |
| **Test connection** | Verifies the URL + key against Sonarr's `system/status`. |
| **Poll interval** | How often to check this Sonarr's grab history (minimum 60s). |
| **Allowed trackers** | Optional allow-list of tracker plugins to auto-import. Leave all unchecked to allow every supported tracker. |
| **Default client / category / download dir** | Applied to every topic this instance auto-creates. Point these at the **same** qBittorrent + category this Sonarr imports from (e.g. `tv-sonarr`) so Sonarr keeps importing Marauder's future downloads. |
| **Replace previous version by default** | `default_replace_on_update`: **Use global default** (`null`), **On**, or **Off** for newly created topics. |
| **Delete the old files by default** | `default_replace_delete_data`: independently inherits the global default (`null`) or overrides it with **On** / **Off**. Disabled in the form when the effective replace setting is known to be off; stays selectable if the global default cannot be read. Both values are shown on the instance card. |
| **Import updates when they finish downloading** | `import_on_complete` (default **off**). When a topic this instance created receives an updated torrent and it finishes downloading, Marauder asks this Sonarr to import the new files. See [Importing updates](#importing-updates). |

The two replace defaults apply only at topic creation: each instance override
wins over **Settings → Topic defaults**, with built-in replace **off** / delete
data **on** if settings cannot be read. Existing topics retain their policy,
even with **Realign existing topics to these defaults** enabled; that option
only realigns the client, category, and directory. Enabling or disabling an
instance preserves its overrides.

For growing season packs that reuse the same folder, set replace **On** and
delete files **Off**, so the new torrent can reuse the downloaded episodes.
See [Replace previous version on update](replace-on-update.md#defaults-for-new-topics).

Each instance lives as its own row in the database (encrypted API key) and is
editable at runtime — no restart, no env vars. Changes take effect on the next
poll tick. **Go-forward on first enable** applies per instance: enabling an
instance stamps *its* cursor to "now" and imports nothing historical.

## Importing updates

Sonarr tracks a download by its infohash. When the tracker later **updates**
a release Sonarr grabbed — a season pack gains an episode — Marauder delivers
the new torrent, but its infohash is one Sonarr never grabbed, so Sonarr's
Completed Download Handling does not reliably pick the new episodes up. With
**Import updates when they finish downloading** on, Marauder closes that loop
for **new episodes**.

When the progress watcher sees a delivery finish (once per delivery, see
`MARAUDER_PROGRESS_WATCHER_ENABLED`), Marauder imports it only if all of these
hold:

- the topic was created by this Sonarr integration and is not in notify-only
  mode;
- the finished torrent is **not** one Sonarr grabbed itself — Marauder
  remembers the last few grabs per topic, so a re-grab does not make an earlier
  one look foreign (Sonarr imports its own grabs; a second import would race
  it);
- the instance that created the topic still exists, is enabled, has the option
  on, and still points at the **same Sonarr server** it pointed at when the
  topic was linked (see below).

Then it:

1. waits ~30 s and re-reads the torrent from the client until it is
   **complete and at rest** (100% of the wanted data, and seeding, stopped or
   queued) — qBittorrent reports 100% while it is still moving files out of its
   temporary folder, and a recheck can find data missing after the fact;
2. reads the torrent's save path and file list from the client. The client's
   paths are the truth: a custom folder name is honoured, and files skipped by
   "download only new files" are left out;
3. translates the path with **Sonarr's own Remote Path Mappings** (see below);
4. asks Sonarr which video files at that path it could import
   (`GET /api/v3/manualimport?folder=…`) and keeps only this torrent's own
   files;
5. has Sonarr re-evaluate those files against the series it grabbed
   (`POST /api/v3/manualimport` with the series id), so a Cyrillic folder name
   that Sonarr cannot parse does not matter;
6. keeps the files with a recognised episode, **no rejection**, and **no
   episode that already has a file in Sonarr**;
7. checks again that the topic and the instance have not changed while it
   worked (deleted, switched to notify-only, option turned off, re-linked, or
   re-pointed at another server) — files evaluated under one link are never
   sent under another;
8. queues Sonarr's `ManualImport` command in **copy** mode — never move, so the
   torrent keeps seeding. If Sonarr's *Use Hardlinks instead of Copy* is on,
   the copy is a hardlink.

### New episodes only

The import never replaces an episode file already in your library. Sonarr
rejects only *downgrades*: a file of the same quality passes its checks, and
importing it deletes the existing file before copying the new one. So
Marauder skips every file covering an episode that already has a file —
quality upgrades and propers of episodes you have are out of scope; Sonarr's
own upgrade handling still deals with those. The timeline entry says how many
files were skipped for this reason.

"Nothing to import" is a normal result: every episode is already in the
library, or Sonarr rejected the files. The reasons are logged.

### Requirements

- **qBittorrent or Transmission.** Only clients that report live status can
  tell Marauder a download finished, and they also report the save path and
  file list. With Deluge, µTorrent or the download folder the option does
  nothing.
- **The progress watcher must be on** (`MARAUDER_PROGRESS_WATCHER_ENABLED`,
  default on).
- **Sonarr must be able to read the files.** If Sonarr sees the download
  directory under a different path than the torrent client (different
  containers or hosts), add a **Remote Path Mapping** in Sonarr
  (Settings → Download Clients). Like Sonarr, Marauder keys a mapping by the
  client's **host** as well as the path. The host it uses is the one in the
  client's URL in Marauder (e.g. `qbittorrent` for `http://qbittorrent:8080`),
  compared case-insensitively and without a port:
  - if any mapping names that host, only those mappings are used, and the
    longest matching remote path wins;
  - if none does (Sonarr may know the client by another name or an IP), the
    path is rewritten only when **every** mapping whose remote path contains it
    — of any length, for any host — rewrites it to the same place. Otherwise
    the import is refused as ambiguous rather than risk applying another
    client's mapping. Use the same host name in Sonarr's download client
    settings as in Marauder's client URL to avoid this;
  - with no matching mapping at all the path is passed unchanged.

### Topics created before this feature, and re-pointed instances

The option needs to know which Sonarr instance and series a topic belongs to.
Topics created from now on carry both, together with a short one-way
fingerprint of the Sonarr URL they were linked against (a series id only means
something on the server that issued it; the URL itself is not stored on the
topic). An older Sonarr-created topic gets the link the next time Sonarr grabs
that release again; until then its updates are skipped (*topic predates the
Sonarr instance/series link*). If an instance's URL is changed to point at
another server, its topics' links go stale (*link_stale*) until a fresh grab
re-links them. Changing only the API key, or the URL's letter case or trailing
slash, does not.

### Interactions

- **Replace previous version on update:** the new torrent is delivered first
  and the old one removed afterwards; the import only ever reads the new
  torrent's files, so deleting the old data does not affect it.
- **Download only new files:** skipped files are never offered to Sonarr.
- **Notify-only topics:** nothing is delivered, so nothing is imported — and a
  topic switched to notify-only while a delivery is still downloading is not
  imported either.
- **Per-episode trackers (LostFilm):** every delivered episode torrent other
  than the ones Sonarr grabbed is imported.
- **Instance deleted or disabled after the topic was created:** the import is
  skipped and logged; the topic itself is unaffected.

### Watching it

Each attempt lands on the topic's timeline as **Sonarr import** (warn-coloured
when it failed) — never as a notification. The metric
`marauder_sonarr_imports_total{result}` counts every decision; `result` is one
of `requested`, `nothing_to_import`, `nothing_found`, `not_sonarr`,
`notify_only`, `own_grab`, `no_link`, `link_stale`, `instance_gone`,
`instance_disabled`, `option_off`, `topic_gone`, `torrent_gone`, `not_ready`,
`no_file_list`, `path_ambiguous`, `dropped`, `error`. Marauder does not follow
the command to completion; Sonarr's **Activity → Queue** and **History** show
its result.

## Trying it out

The dev overlay bundles a ready-to-use *arr stack
(`deploy/docker-compose.dev.yml` starts Prowlarr + Sonarr + FlareSolverr), and
`deploy/docker-compose.arr.yml` attaches the same stack to an **already-running**
Marauder so you can add it without rebuilding. See
[deploy/docker-compose.arr.yml](../deploy/docker-compose.arr.yml) for the
attach-to-existing flow.

## Notes

- **Sonarr must actually grab.** The integration reacts to Sonarr's *grabbed*
  history — it doesn't search or grab on its own. In the normal Seerr → Sonarr
  flow, "search on add" produces the grab automatically.
- **Russian-only / anime releases sometimes fail Sonarr's auto-grab** with
  "Unable to parse release" (the title doesn't map to Sonarr's episode model).
  Those releases still exist on the tracker — use Sonarr's **Interactive
  Search** and grab one manually (Sonarr will ask to override the rejection).
  Marauder picks up the resulting grab the same way.
- **Credentials.** Auto-created topics for trackers that need a login (e.g.
  Kinozal, RuTracker) won't be able to download until you add that tracker
  account under **Accounts**; the topic is still created and the scheduler
  surfaces the missing-credential state. RuTracker can fall back to the
  public topic magnet without an account, but that magnet carries no announce
  URL, so add the account to get the real `.torrent`.
- **Owner.** Auto-created topics are owned by the admin who saved the instance
  (falling back to the first admin), since the poller runs headless.
