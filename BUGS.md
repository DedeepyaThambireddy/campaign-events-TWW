# Part 3: bugs in `debugging/main.go`

Verification used for every fix: `go run . events.jsonl | diff - expected_output.txt` (empty),
`go run -race .` (clean), and 20 repeated runs producing one identical md5. The original code
reports 4 DATA RACE warnings under `-race`; the fixed code reports none.

## Bug 1: duplicates are only removed within a batch
- **What:** `processBatch` created `seen := make(map[string]bool)` on every call, so an `event_id`
  repeated in a *different* 200-line batch was counted again. Totals were inflated
  (e.g. cmp_A sent=2025 vs expected 1987).
- **Why:** The dedup set was scoped to one batch; the requirement is "across the whole file".
- **Fix:** One package-level `seenIDs` map, used in place of the per-batch `seen` (it is only touched
  from the main goroutine, in the sequential loop, so it needs no lock).
- **Verified:** sent/delivered/opened/clicked totals now match expected.

## Bug 2: `unique_opens` is keyed by contact only, not (campaign, contact)
- **What:** `openedBy[ev.ContactID]` is global, so a contact who opened in campaign A was never counted
  as a unique opener in campaign B. unique_opens was far too low (cmp_A 549 vs 942).
- **Why:** The spec says the same contact opening in two campaigns counts once *in each*; the key ignored the campaign.
- **Fix:** key is `ev.CampaignID + "|" + ev.ContactID`.
- **Verified:** unique_opens matches for all five campaigns.

## Bug 3: daily delivered buckets use the machine's local time zone
- **What:** `ev.Timestamp.Local()` buckets by the host's zone, so events near midnight UTC land on
  the wrong day on any non-UTC machine. It may not show on a UTC machine, but it's still wrong.
- **Why:** `time.Time` parsed from `...Z` is UTC, but `.Local()` converts it before formatting.
- **Fix:** `.UTC()`.
- **Verified:** ran with `TZ=America/Los_Angeles` and default: both match expected. (Bucket totals
  only matched expected after bugs 1 and 2 were also fixed.)

## Bug 4: data race on the counters (non-deterministic output)
- **What:** 8 worker goroutines call `apply()`, which does `cs.Sent++` etc. on the same
  `*CampaignStats` with no synchronisation. `x++` is read-modify-write, so concurrent increments
  can be lost, and the totals can differ from run to run.
- **Why:** The comments assume "the increments are cheap" and that the maps are the only unsafe part;
  but plain ints shared between goroutines are not safe either. `-race` flags it.
- **Fix:** a package-level `sync.Mutex` taken inside `apply()`. (Alternatives: atomics, or
  dropping the pool, since the work per event is trivial; a mutex is the smallest diff.)
- **Verified:** `go run -race .` before: 4 DATA RACE reports; after: none. 20 runs -> one distinct output hash.
