package main

import (
	"sync"
	"time"
)

type Outcome int

const (
	Accepted  Outcome = iota // new event, counted
	Duplicate                // same event_id, same content: ignored
	Conflict                 // same event_id, different content: ignored, first write wins
)

type campaignAgg struct {
	sent, delivered, opened, clicked int
	openers, clickers                map[string]struct{} // distinct contacts
	lastEventAt                      time.Time           // max provider timestamp
	events                           []*Event            // arrival order, for listing
}

// Store is the in-memory persistence layer. One RWMutex guards everything:
// at a few thousand events/day contention is irrelevant, and a single lock
// makes "check for duplicate, then count" trivially atomic.
type Store struct {
	mu    sync.RWMutex
	seen  map[string]*Event // event_id -> first accepted event
	camps map[string]*campaignAgg
}

func NewStore() *Store {
	return &Store{seen: map[string]*Event{}, camps: map[string]*campaignAgg{}}
}

// sameEvent defines "same event" for conflict detection: identical on every
// field that affects counting. Metadata is deliberately excluded: a retry
// that differs only in free-form metadata is still the same event.
func sameEvent(a, b *Event) bool {
	return a.CampaignID == b.CampaignID && a.ContactID == b.ContactID &&
		a.Type == b.Type && a.Timestamp.Equal(b.Timestamp)
}

// Ingest applies a batch under one lock so a concurrent stats read never
// observes half a batch, and the duplicate check + counter update are atomic.
func (s *Store) Ingest(evs []Event) []Outcome {
	out := make([]Outcome, len(evs))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range evs {
		ev := evs[i]
		if prev, ok := s.seen[ev.EventID]; ok {
			if sameEvent(prev, &ev) {
				out[i] = Duplicate
			} else {
				out[i] = Conflict
			}
			continue
		}
		e := &ev
		s.seen[ev.EventID] = e
		c := s.camps[ev.CampaignID]
		if c == nil {
			c = &campaignAgg{openers: map[string]struct{}{}, clickers: map[string]struct{}{}}
			s.camps[ev.CampaignID] = c
		}
		switch ev.Type {
		case "sent":
			c.sent++
		case "delivered":
			c.delivered++
		case "opened":
			c.opened++
			c.openers[ev.ContactID] = struct{}{}
		case "clicked":
			c.clicked++
			c.clickers[ev.ContactID] = struct{}{}
		}
		if ev.Timestamp.After(c.lastEventAt) {
			c.lastEventAt = ev.Timestamp
		}
		c.events = append(c.events, e)
		out[i] = Accepted
	}
	return out
}

type Stats struct {
	Sent, Delivered, Opened, Clicked int
	UniqueOpens, UniqueClicks        int
	LastEventAt                      time.Time
}

// Stats returns a consistent snapshot, or ok=false if the campaign is unknown.
func (s *Store) Stats(campaignID string) (Stats, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.camps[campaignID]
	if c == nil {
		return Stats{}, false
	}
	return Stats{c.sent, c.delivered, c.opened, c.clicked,
		len(c.openers), len(c.clickers), c.lastEventAt}, true
}

// List returns up to limit events, newest-arrival first. cursor is the number
// of events already seen from the newest end ("before" position in arrival
// order); it is stable under concurrent inserts because we page by arrival
// index counted from the start, not by offset from the newest.
// beforeIdx == -1 means start from the newest.
func (s *Store) List(campaignID string, beforeIdx, limit int) (items []Event, nextBefore int, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.camps[campaignID]
	if c == nil {
		return nil, 0, false
	}
	end := len(c.events)
	if beforeIdx >= 0 && beforeIdx < end {
		end = beforeIdx
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	for i := end - 1; i >= start; i-- {
		items = append(items, *c.events[i])
	}
	if start == 0 {
		return items, -1, true
	}
	return items, start, true
}
