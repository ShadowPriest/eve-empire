package esi

// Corporation structures and the character notifications that report
// attacks on them. The structures route needs the Station Manager role
// (403 otherwise); notifications are personal and cache for 10 minutes.

import (
	"fmt"
	"time"
)

// StructureService is one service module and whether it runs.
type StructureService struct {
	Name  string `json:"name"`
	State string `json:"state"` // online|offline|cleanup
}

// CorpStructure is one structure the corporation owns, with fuel and
// vulnerability state. Optional dates are pointers: absent fuel_expires
// means the structure is out of fuel (low power).
type CorpStructure struct {
	StructureID   int64  `json:"structure_id"`
	TypeID        int64  `json:"type_id"`
	TypeName      string `json:"-"`
	CorporationID int64  `json:"corporation_id"`
	SystemID      int64  `json:"system_id"`
	Name          string `json:"name"`
	// State: shield_vulnerable is the normal condition; armor_reinforce /
	// hull_reinforce mean the structure has been attacked into reinforce.
	State              string             `json:"state"`
	StateTimerStart    *time.Time         `json:"state_timer_start"`
	StateTimerEnd      *time.Time         `json:"state_timer_end"`
	FuelExpires        *time.Time         `json:"fuel_expires"`
	UnanchorsAt        *time.Time         `json:"unanchors_at"`
	ReinforceHour      *int               `json:"reinforce_hour"`
	NextReinforceHour  *int               `json:"next_reinforce_hour"`
	NextReinforceApply *time.Time         `json:"next_reinforce_apply"`
	Services           []StructureService `json:"services"`
}

// CorporationStructures lists the corporation's structures. Needs the
// Station Manager role on the requesting character.
func (c *Client) CorporationStructures(characterID, corporationID int64) ([]CorpStructure, error) {
	var out []CorpStructure
	page := 1
	for {
		var chunk []CorpStructure
		pages, err := c.get(characterID,
			fmt.Sprintf("/corporations/%d/structures/?page=%d", corporationID, page), &chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if page >= pages || len(chunk) == 0 {
			break
		}
		page++
	}
	ids := make([]int64, len(out))
	for i, st := range out {
		ids[i] = st.TypeID
	}
	names := c.typeNamesLocalized(ids)
	for i := range out {
		out[i].TypeName = names[out[i].TypeID]
	}
	return out, nil
}

// Notification is one character notification. Text is CCP's YAML-ish
// key: value blob — the only machine-readable part of an attack report.
type Notification struct {
	NotificationID int64     `json:"notification_id"`
	SenderID       int64     `json:"sender_id"`
	SenderType     string    `json:"sender_type"`
	Text           string    `json:"text"`
	Timestamp      time.Time `json:"timestamp"`
	Type           string    `json:"type"`
	IsRead         bool      `json:"is_read"`
}

// Notifications returns the character's recent notifications (ESI keeps
// them for a while; cache 600 s).
func (c *Client) Notifications(characterID int64) ([]Notification, error) {
	var out []Notification
	_, err := c.get(characterID, fmt.Sprintf("/characters/%d/notifications/", characterID), &out)
	return out, err
}
