package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxIDLen = 128

// parseEvent turns one raw JSON array element into a validated Event.
// Policy (see NOTES.md): reject anything we cannot count correctly rather
// than guessing. Normalise only what is unambiguous (whitespace, type case,
// timezone offset -> UTC). Unknown extra fields are ignored so providers can
// add fields without breaking us.
func parseEvent(raw json.RawMessage) (Event, string, error) {
	var r rawEvent
	if err := json.Unmarshal(raw, &r); err != nil {
		return Event{}, "", fmt.Errorf("malformed event: %v", err)
	}
	id := strings.TrimSpace(r.EventID)
	// Return the id (if any) even on failure so the caller can report it.
	if id == "" {
		return Event{}, "", fmt.Errorf("event_id is required")
	}
	if len(id) > maxIDLen {
		return Event{}, id, fmt.Errorf("event_id longer than %d chars", maxIDLen)
	}
	camp := strings.TrimSpace(r.CampaignID)
	if camp == "" || len(camp) > maxIDLen {
		return Event{}, id, fmt.Errorf("campaign_id is required (max %d chars)", maxIDLen)
	}
	contact := strings.TrimSpace(r.ContactID)
	if contact == "" || len(contact) > maxIDLen {
		return Event{}, id, fmt.Errorf("contact_id is required (max %d chars)", maxIDLen)
	}
	typ := strings.ToLower(strings.TrimSpace(r.Type))
	if !validTypes[typ] {
		return Event{}, id, fmt.Errorf("unknown event type %q", r.Type)
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(r.Timestamp))
	if err != nil {
		return Event{}, id, fmt.Errorf("timestamp must be RFC3339 (e.g. 2026-08-10T09:15:04Z)")
	}
	return Event{
		EventID: id, CampaignID: camp, ContactID: contact, Type: typ,
		Timestamp: ts.UTC(), Metadata: r.Metadata,
	}, id, nil
}
