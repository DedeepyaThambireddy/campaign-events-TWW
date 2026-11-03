# AI usage

## Tools I used
Claude (the chat app). I didn't use any other AI tools for this assignment.

## What I used it for
I used it for most of the work: reading the brief and the seed data, writing the first version of the
service and its tests, finding and fixing the four bugs in `debugging/`, and drafting the written
sections (NOTES.md, BUGS.md). I then went through the code and notes myself so I could explain them.
It also ran the tests, the `-race` check and some curl calls, and re-checked the seed results with a
separate Python script (211 accepted / 15 duplicates / 3 conflicts / 6 rejected, same as the Go service).

## A suggestion I rejected or changed
SQLite for storage. It's the obvious choice, but it needs a third-party driver, which makes `go run .`
less likely to work first time on the reviewer's machine. At a few thousand events a day, an in-memory
map behind one lock is simpler, and I can fully explain how dedup and counting stay correct. I kept
in-memory and wrote the "data is lost on restart" trade-off in NOTES.md.
I also changed how bad batches are answered: instead of returning an error when some events are
invalid, the service returns 200 with a per-event report, because providers retry on any non-2xx and
retrying a malformed event can never fix it.

## One thing AI helped me understand
That "opened" can mean two different things: every open event, or the number of distinct people who
opened. That's why the stats return both. It also showed me that deduplicating by `event_id` isn't the
whole story: the seed file has the same `event_id` sent twice with different content (for example one
as "sent" and one as "clicked"). I had to decide what to do, and I chose to keep the first and report
the second as a conflict.

## Anything AI generated that I had to debug
Nothing needed real debugging. The service compiled and the tests passed on the first run.
The one thing I did check carefully was the race condition in the debugging exercise. It doesn't
reliably show up in the output, so I confirmed it with `go run -race` (it reported 4 data races before
the fix and none after) instead of trusting that the numbers looked right.
