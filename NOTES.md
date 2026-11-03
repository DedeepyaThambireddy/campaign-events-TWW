# NOTES

## Part 1: Read and think first

### My interpretation
Relay hands messages to providers; providers call our webhook as each message progresses. I own the
service that (a) receives those events safely and (b) answers per-campaign numbers for a live dashboard.
The hard part isn't the HTTP. It's that the input is unreliable (retries, out-of-order, late, malformed),
while the output (numbers marketers act on) has to be trustworthy and not drift.

### Assumptions
- `event_id` is unique per provider event and stable across retries, so it is the identity key.
  (We don't get a provider field, so I treat ids as globally unique.)
- Counts are by event *type*, independent of order. An `opened` arriving before `delivered` is simply counted.
- `timestamp` is provider event time (UTC); it is data about the event, not about arrival, and I don't use it for dedup.
- A single process is fine today (few thousand events/day, ~50 campaigns). Durability across restart is not required for this exercise.
- Dashboard is read-heavy and wants cheap reads: aggregates are maintained at write time.
- Unknown extra JSON fields from providers should be ignored, not rejected.

### Ambiguities I noticed
- "Opened": total open events, or distinct people who opened? Marketers usually think the latter. I return both.
- "Delivered/opened/clicked" are not a strict funnel (opens can arrive before deliveries; clicks without recorded opens), so rates can exceed 100% transiently. I don't clamp.
- What counts as the "same" event: same `event_id` only? What if the same id arrives with different content (seed has 3)?
- Stats for what window? All-time per campaign? Per day? No time range was specified.
- Which timestamp to bucket on (event time vs arrival time) once time-windowing exists?
- Is an unknown campaign an error or just zeros?
- Is losing data on restart acceptable? Are late events of any age accepted?
- Do bots / privacy-proxy opens (e.g. mail privacy protection) count as opens?
- Does `sent` come from providers or from our own system? (Brief says providers send all four.)

### Questions I'd ask the PM
1. Should "opened" on the dashboard be unique people or total events? Same for clicked.
2. What should happen when a provider reuses an event_id with different content: alert, overwrite, or ignore?
3. Does the dashboard need time ranges (today, last 7 days) or only all-time per campaign?
4. How late can an event be and still count? Do we ever restate numbers?
5. Must numbers survive a restart / how fresh must they be (seconds vs minutes)?
6. How many providers, and are event ids guaranteed unique across them?
7. Should dashboard figures ever go down? (see Part 5)

### What I prioritised, in order
1. Correct, idempotent ingest (dedup + per-item validation + concurrency safety).
2. Stats endpoint with explicit semantics (both total and unique).
3. Tests for the behaviours I'd be most embarrassed to break (dup, partial batch, conflict, concurrency, seed).
4. Part 3 (mechanical, verifiable), then Parts 4 and 5, then the optional events-list endpoint (done, small).

### Decisions (Part 2)
- **Same event** = same `event_id`. If a repeat has identical campaign/contact/type/timestamp it is a
  *duplicate* (ignored, counted in `duplicates`). If it differs, it is a *conflict*: **first write wins**,
  second is ignored, reported in the response and logged. Metadata is excluded from the comparison
  (a retry with different free-form metadata is still the same event). Why first-wins: it's deterministic
  and never silently changes numbers already shown; the downside is that arrival order picks the winner
  and the wrong version could win. I'd rather surface it (log/metric) than guess. Seed has 3 such cases.
- **Responses:** body not an array -> 400; oversized -> 413; otherwise **200** with a per-item report.
  Providers retry on non-2xx, and retrying a malformed item can't fix it, so non-2xx would only cause
  retry storms. Duplicates are 200 too (the retry succeeded in the sense that matters: we have the event).
- **Validation:** reject missing/empty `event_id`, `campaign_id`, `contact_id`; unknown `type`
  (e.g. `spam_report`); non-RFC3339 timestamps (e.g. `10-08-2026 09:00`, `not-a-time`). Normalise only
  the unambiguous: whitespace, type case (`OPENED` -> `opened`), offsets -> UTC. Seed: 6 rejected.
  Each element is decoded separately (`json.RawMessage`) so one bad element can't fail the other 199.
- **Metrics:** `totals.opened` = deduplicated open *events*; `unique.opened` = distinct *contacts* who opened.
  Same for clicks. Rates use unique/delivered; null when denominator is 0.
- **Storage:** in-memory. `seen` map (event_id -> event) for idempotency + per-campaign aggregates updated at
  ingest (O(1) reads, no scans), plus an arrival-ordered slice for the events list. Why: simplest thing whose
  correctness I can fully argue at this volume; stdlib only so `go run .` works. Cost: data is lost on
  restart (see "another day").
- **Concurrency:** one `RWMutex`; each batch is ingested under one write lock, so dedup-check + count is atomic
  and a stats read never sees half a batch. Tested with 32 goroutines posting the same batch under `-race`.
- **Unknown campaign:** 404 (distinguishes typo from "no events yet" only imperfectly; a campaign
  registry would fix it properly).

## Part 4: Scale memo (100M events/day ~ 1,200/s average, maybe 5-10x that at peak)

- **What breaks first:** memory and durability. The `seen` map holds every event forever (100M/day is
  tens of GB/day) and it all vanishes on restart. Close behind: the single global lock serialising all writes,
  and the per-campaign `openers`/`clickers` sets that grow with audience size. A single hot campaign
  (the big client) becomes the bottleneck.
- **What I'd change, in order:** (1) persist events to a real DB with a unique constraint on `event_id`
  (`INSERT ... ON CONFLICT DO NOTHING`), making dedup durable and the DB the source of truth;
  (2) take dedup out of process memory; (3) make aggregates incremental in the DB (rollup rows per
  campaign/day, updated only when the insert actually inserted a row); (4) shard locks / partition by
  campaign_id; (5) then a queue.
- **API contract:** POST and GET shapes can stay the same; I'd keep returning counts. Possibly add time-range
  filters and "as of" semantics. Storage design *would* change: append-only events table + rollups, unique
  counts via a DB `COUNT(DISTINCT)` on rollup keys or approximate (HyperLogLog) at huge audience sizes.
- **Queue?** Yes, eventually: ack the provider quickly (fast 2xx stops retries), absorb spikes, process async.
  New problems: delivery is at-least-once so the consumer must be idempotent; ordering is not guaranteed
  across partitions (partition by campaign_id or event_id); backpressure and poison messages (dead-letter
  queue); and the dashboard becomes eventually consistent, so "accepted" no longer means "counted".
- **Where duplicates can now sneak in:** queue redelivery after a consumer crash/timeout before ack;
  retries from providers hitting different instances at the same time (needs DB-level uniqueness, not
  in-memory); replays/backfills; consumer writes applied twice if the rollup update and the event insert aren't one transaction.
- **Keeping counters trustworthy:** rollups updated in the same transaction as the insert that actually
  inserted; a periodic reconciliation job recomputing counts from raw events and comparing to rollups; counters
  derived from events (rebuildable), never the other way round; keep `received_at` so numbers can be explained "as of".
- **Monitor:** ingest rate and p99 latency; 4xx/5xx rate; duplicate ratio and conflict count (a spike means a
  provider issue); rejected-by-reason; queue depth / consumer lag; event lag (received_at - timestamp);
  DB size/insert latency; reconciliation drift between rollups and raw; stats-endpoint latency.
- **Deliberately not solving yet:** exactly-once end to end (idempotency is enough); multi-region;
  exact distinct counts for giant audiences (accept approximation); per-contact analytics; auth/rate limiting;
  retention/archival policy; "correct" handling of late events past some age (I'd first measure how late they actually are).

## Part 5: The angry marketer

Opened fell 250,000 -> 248,000 while Delivered rose 970,000 -> 975,000 and Sent/Clicked didn't move.

### Plausible explanations (before assuming a bug)
**Delivered rising (expected):**
1. Late delivery events: providers/carriers report deliveries hours later; soft-bounced messages are retried and finally delivered.
2. Provider outage or backlog replayed events after a delay. 
3. Retries finally got through after we returned errors earlier.

**Opened falling (needs an explanation, since a pure append-only count can't go down):**
4. The metric changed meaning: e.g. "unique opens" vs "total opens", or a deploy changed the definition.
5. Opens are being reclassified: bot/security-scanner or Apple Mail Privacy Protection filtering applied retroactively.
6. Rolling window: the dashboard counts opens in the "last N hours", so older opens age out.
7. Deleted/corrected data: GDPR deletion, unsubscribe purge, contact list cleanup, or a duplicate-cleanup job
   removing rows that were previously double counted.
8. Dedup/conflict logic changed or ran: a duplicate previously counted now collapses (e.g. same event re-sent under a new id was merged).
9. Read-path inconsistency: stale cache, lagging replica, or one of several shards/instances not responding (partial sum).
10. Data loss: service restart with in-memory state, failed partial restore, dropped queue partition.
11. Rows moved between campaigns (contacts or campaign ids corrected), so opens left this campaign.
12. Provider corrected itself (sent opens it later retracted).
13. Display issue: the two screenshots come from different filters, time zones, or comparing different dashboards.
14. Real bug: counter decrement on some path, double-count bug fixed in a deploy, integer/overflow handling, race in aggregation.
(Also: Sent is exactly 1,000,000 both times: probably just a round campaign size, but I'd check it isn't a display cap.)

### What I'd check first
1. Does the **API** return the same numbers the dashboard shows? (separates front-end/cache from backend)
2. Between 10:00 and 10:30: deploys, restarts, migrations, cleanup jobs, shard/replica health.
3. Recount from raw events: distinct `opened` event ids (and distinct contacts) for that campaign. Is the raw
   set monotonic? That needs `received_at`. **Our current service doesn't store it** (only provider timestamp),
   which is a gap I'd fix, since I can't otherwise answer "what did we know at 10:00".
4. Whether the metric is total or unique opens and whether any time window applies.

### Bug or expected?
- If raw rows are append-only and the raw count never decreased but the dashboard did -> **bug** in read path
  (cache/window/replica/definition).
- If raw rows were deleted/reclassified by a known, documented process (bot filtering, deletions) -> **expected
  behaviour but a product problem**: numbers shouldn't silently go down without an explanation, so show a note / restate.
- If raw rows decreased without any intended process -> **bug / data loss**; treat as an incident.
### Is Delivered rising suspicious?
Not by itself: delivery is long-tail and lags sent (97.0% -> 97.5%). It would be suspicious if delivered exceeded
sent, kept rising days after the send, or rose with no matching provider events/log lines.

## What I completed / what I intentionally skipped
**Completed:** Parts 1-5; POST /events (per-item validation, idempotent dedup, conflict handling, 200-with-report),
GET stats, and the optional GET events with cursor pagination; tests (incl. seed and `-race` concurrency);
all four bugs in `debugging/`.
**Skipped on purpose:** persistence (SQLite/WAL): needs a driver and wasn't the priority; time-range
stats; campaign registry (so unknown campaign is 404); auth/rate limiting; metrics/structured logging;
a max batch-size cap (only a 5 MiB body cap); checking for implausible future timestamps.

## If I had another day
- Durable storage (SQLite with `event_id` primary key), and store `received_at` alongside provider `timestamp`.
- Time-range and daily stats; `GET /stats?from=&to=`.
- Persist conflicts and rejects in a "dead letter" table for inspection instead of only logging.
- Property test: random shuffles/duplicates of an event set must always produce the same stats.
- Load test for lock contention; decide on sharding by campaign.
- Confirm answers to the PM questions above, especially unique vs total opens and conflict policy.
