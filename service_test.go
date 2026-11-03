package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func post(t *testing.T, h http.Handler, body string) (int, ingestResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/events", strings.NewReader(body)))
	var resp ingestResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp
}

func stats(t *testing.T, h http.Handler, id string) (int, statsResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/campaigns/"+id+"/stats", nil))
	var s statsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &s)
	return rr.Code, s
}

func ev(id, camp, contact, typ string) string {
	return fmt.Sprintf(`{"event_id":%q,"campaign_id":%q,"contact_id":%q,"type":%q,"timestamp":"2026-08-10T09:00:00Z"}`, id, camp, contact, typ)
}

// The core promise: a retried event never changes the numbers.
func TestDuplicateDoesNotDoubleCount(t *testing.T) {
	h := (&Server{store: NewStore()}).Routes()
	body := "[" + ev("e1", "c", "u1", "delivered") + "]"
	post(t, h, body)
	code, resp := post(t, h, body)
	if code != 200 || resp.Duplicates != 1 || resp.Accepted != 0 {
		t.Fatalf("retry: code=%d resp=%+v", code, resp)
	}
	if _, s := stats(t, h, "c"); s.Totals.Delivered != 1 {
		t.Fatalf("delivered=%d want 1", s.Totals.Delivered)
	}
}

// One bad element must not sink the other good ones, and must not cause a
// non-2xx (which would make the provider retry the whole batch forever).
func TestMixedBatchPartialSuccess(t *testing.T) {
	h := (&Server{store: NewStore()}).Routes()
	body := "[" + ev("e1", "c", "u1", "sent") + `,{"event_id":"e2","campaign_id":"c","type":"sent","timestamp":"nope"},` +
		`{"event_id":5},"junk",` + ev("e3", "c", "u2", "sent") + "]"
	code, resp := post(t, h, body)
	if code != 200 || resp.Accepted != 2 || resp.Rejected != 3 || resp.Received != 5 {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
	if _, s := stats(t, h, "c"); s.Totals.Sent != 2 {
		t.Fatalf("sent=%d want 2", s.Totals.Sent)
	}
	if code, _ := post(t, h, `{"not":"an array"}`); code != 400 {
		t.Fatalf("non-array body: code=%d want 400", code)
	}
}

// Same event_id with different content: first write wins, second is reported.
func TestConflictKeepsFirst(t *testing.T) {
	h := (&Server{store: NewStore()}).Routes()
	post(t, h, "["+ev("e1", "c", "u1", "sent")+"]")
	_, resp := post(t, h, "["+ev("e1", "c", "u1", "clicked")+"]")
	if resp.Conflicts != 1 || resp.Accepted != 0 {
		t.Fatalf("resp=%+v", resp)
	}
	_, s := stats(t, h, "c")
	if s.Totals.Sent != 1 || s.Totals.Clicked != 0 {
		t.Fatalf("stats=%+v", s.Totals)
	}
}

// Out-of-order arrival: opened before delivered gives the same numbers.
// Repeat opens by one contact: opened counts events, unique.opened counts people.
func TestOutOfOrderAndUniqueOpens(t *testing.T) {
	h := (&Server{store: NewStore()}).Routes()
	post(t, h, "["+ev("o1", "c", "u1", "opened")+","+ev("o2", "c", "u1", "opened")+"]")
	post(t, h, "["+ev("d1", "c", "u1", "delivered")+"]")
	_, s := stats(t, h, "c")
	if s.Totals.Opened != 2 || s.Unique.Opened != 1 || s.Totals.Delivered != 1 {
		t.Fatalf("stats=%+v %+v", s.Totals, s.Unique)
	}
}

// Same batch delivered by many concurrent requests: counted exactly once.
// Run with -race.
func TestConcurrentDuplicates(t *testing.T) {
	h := (&Server{store: NewStore()}).Routes()
	var batch []string
	for i := 0; i < 50; i++ {
		batch = append(batch, ev(fmt.Sprintf("e%d", i), "c", fmt.Sprintf("u%d", i), "opened"))
	}
	body := "[" + strings.Join(batch, ",") + "]"
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, h, body)
			stats(t, h, "c")
		}()
	}
	wg.Wait()
	_, s := stats(t, h, "c")
	if s.Totals.Opened != 50 || s.Unique.Opened != 50 {
		t.Fatalf("opened=%d unique=%d want 50/50", s.Totals.Opened, s.Unique.Opened)
	}
}

// The provided seed file must be survivable, and the books must balance.
func TestSeedFile(t *testing.T) {
	data, err := os.ReadFile("seed/events.json")
	if err != nil {
		t.Skip("no seed file")
	}
	h := (&Server{store: NewStore()}).Routes()
	code, r := post(t, h, string(data))
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if r.Accepted+r.Duplicates+r.Conflicts+r.Rejected != r.Received {
		t.Fatalf("outcomes do not sum to received: %+v", r)
	}
	// Loading the same file again changes nothing.
	_, before := stats(t, h, "cmp_summer_sale")
	_, r2 := post(t, h, string(data))
	if r2.Accepted != 0 {
		t.Fatalf("re-post accepted %d new events", r2.Accepted)
	}
	_, after := stats(t, h, "cmp_summer_sale")
	if before.Totals != after.Totals || before.Unique != after.Unique {
		t.Fatalf("re-post changed stats")
	}
	t.Logf("seed: %+v", struct{ A, D, C, R int }{r.Accepted, r.Duplicates, r.Conflicts, r.Rejected})
}

func TestPaginationCoversAllOnce(t *testing.T) {
	s := NewStore()
	var evs []Event
	for i := 0; i < 7; i++ {
		e, _, err := parseEvent([]byte(ev(fmt.Sprintf("e%d", i), "c", "u", "sent")))
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	s.Ingest(evs)
	seen := map[string]bool{}
	cur := -1
	for {
		items, next, _ := s.List("c", cur, 3)
		for _, e := range items {
			if seen[e.EventID] {
				t.Fatalf("dup in pagination: %s", e.EventID)
			}
			seen[e.EventID] = true
		}
		if next < 0 {
			break
		}
		cur = next
	}
	if len(seen) != 7 {
		t.Fatalf("saw %d want 7", len(seen))
	}
}
