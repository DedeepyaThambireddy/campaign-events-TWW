package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
)

const maxBodyBytes = 5 << 20 // 5 MiB per request

func main() {
	srv := &Server{store: NewStore()}
	addr := ":8080"
	fmt.Printf("listening on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}

type Server struct{ store *Store }

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", s.handlePostEvents)
	mux.HandleFunc("GET /campaigns/{campaignID}/stats", s.handleGetStats)
	mux.HandleFunc("GET /campaigns/{campaignID}/events", s.handleListEvents)
	return mux
}

type itemError struct {
	Index   int    `json:"index"`
	EventID string `json:"event_id,omitempty"`
	Reason  string `json:"reason"`
	Kind    string `json:"kind"` // "rejected" | "conflict"
}

type ingestResponse struct {
	Received   int         `json:"received"`
	Accepted   int         `json:"accepted"`
	Duplicates int         `json:"duplicates"`
	Conflicts  int         `json:"conflicts"`
	Rejected   int         `json:"rejected"`
	Errors     []itemError `json:"errors"`
}

// handlePostEvents ingests a JSON array of events.
//
// Status codes: providers retry on anything that is not 2xx, and retrying a
// malformed event can never fix it. So once the body is a readable JSON array
// we always return 200 and report per-item outcomes in the body. Only a body
// we cannot interpret as an array at all gets 4xx.
func (s *Server) handlePostEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read body"})
		return
	}
	// Decode to RawMessage per element so one bad element cannot fail the batch.
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a JSON array of events"})
		return
	}

	resp := ingestResponse{Received: len(raws), Errors: []itemError{}}
	valid := make([]Event, 0, len(raws))
	validIdx := make([]int, 0, len(raws)) // position in the original array
	for i, raw := range raws {
		ev, id, err := parseEvent(raw)
		if err != nil {
			resp.Rejected++
			resp.Errors = append(resp.Errors, itemError{Index: i, EventID: id, Reason: err.Error(), Kind: "rejected"})
			continue
		}
		valid = append(valid, ev)
		validIdx = append(validIdx, i)
	}

	for j, o := range s.store.Ingest(valid) {
		switch o {
		case Accepted:
			resp.Accepted++
		case Duplicate:
			resp.Duplicates++
		case Conflict:
			resp.Conflicts++
			log.Printf("conflict: event_id %s re-sent with different content", valid[j].EventID)
			resp.Errors = append(resp.Errors, itemError{
				Index: validIdx[j], EventID: valid[j].EventID, Kind: "conflict",
				Reason: "event_id already stored with different content; first version kept",
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type statsResponse struct {
	CampaignID  string     `json:"campaign_id"`
	Totals      totals     `json:"totals"`
	Unique      unique     `json:"unique"`
	Rates       rates      `json:"rates"`
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	AsOf        time.Time  `json:"as_of"`
}
type totals struct {
	Sent      int `json:"sent"`
	Delivered int `json:"delivered"`
	Opened    int `json:"opened"`  // all deduplicated open events
	Clicked   int `json:"clicked"` // all deduplicated click events
}
type unique struct {
	Opened  int `json:"opened"`  // distinct contacts who opened
	Clicked int `json:"clicked"` // distinct contacts who clicked
}
type rates struct { // null when the denominator is 0; not clamped to 1.0
	DeliveryRate *float64 `json:"delivery_rate"` // delivered / sent
	OpenRate     *float64 `json:"open_rate"`     // unique opened / delivered
	ClickRate    *float64 `json:"click_rate"`    // unique clicked / delivered
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := math.Round(float64(n)/float64(d)*10000) / 10000
	return &v
}

func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("campaignID")
	st, ok := s.store.Stats(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown campaign"})
		return
	}
	last := st.LastEventAt
	writeJSON(w, http.StatusOK, statsResponse{
		CampaignID: id,
		Totals:     totals{st.Sent, st.Delivered, st.Opened, st.Clicked},
		Unique:     unique{st.UniqueOpens, st.UniqueClicks},
		Rates: rates{
			DeliveryRate: ratio(st.Delivered, st.Sent),
			OpenRate:     ratio(st.UniqueOpens, st.Delivered),
			ClickRate:    ratio(st.UniqueClicks, st.Delivered),
		},
		LastEventAt: &last,
		AsOf:        time.Now().UTC(),
	})
}

// handleListEvents: newest-arrival first. ?limit=1..200 (default 50),
// ?cursor=<opaque int from next_cursor>.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be 1..200"})
			return
		}
		limit = n
	}
	before := -1
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		before = n
	}
	items, next, ok := s.store.List(r.PathValue("campaignID"), before, limit)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown campaign"})
		return
	}
	resp := map[string]any{"events": items}
	if next >= 0 {
		resp["next_cursor"] = strconv.Itoa(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
