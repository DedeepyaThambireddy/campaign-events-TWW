package main

import "time"

// Event is a validated, normalised provider event as stored by the service.
type Event struct {
	EventID    string         `json:"event_id"`
	CampaignID string         `json:"campaign_id"`
	ContactID  string         `json:"contact_id"`
	Type       string         `json:"type"`
	Timestamp  time.Time      `json:"timestamp"` // UTC, provider time
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// rawEvent is what we decode each array element into. Timestamp stays a
// string so we can give a precise validation error instead of a decode error.
type rawEvent struct {
	EventID    string         `json:"event_id"`
	CampaignID string         `json:"campaign_id"`
	ContactID  string         `json:"contact_id"`
	Type       string         `json:"type"`
	Timestamp  string         `json:"timestamp"`
	Metadata   map[string]any `json:"metadata"`
}

var validTypes = map[string]bool{"sent": true, "delivered": true, "opened": true, "clicked": true}
