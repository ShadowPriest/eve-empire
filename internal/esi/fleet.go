package esi

// Fleet endpoints — the only sizeable write surface in ESI. Everything
// here bypasses the cache (see Client.call): the reads live five seconds
// and the writes must not be replayed from a cached body.
//
// Practical limits, all discovered by CCP rather than chosen by us:
//   - a fleet cannot be created or disbanded through ESI, only managed;
//   - there are no broadcasts, no fleet warp, no fleet advertisement;
//   - only the fleet boss gets past /fleets/{id}/ — everyone else is
//     answered 404 "The fleet does not exist or you don't have access".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Roles as ESI spells them.
const (
	RoleFleetCommander = "fleet_commander"
	RoleWingCommander  = "wing_commander"
	RoleSquadCommander = "squad_commander"
	RoleSquadMember    = "squad_member"
)

// FleetRef is /characters/{id}/fleet/: which fleet the character is in
// and where they sit in it. WingID/SquadID are -1 when not applicable.
type FleetRef struct {
	FleetID int64  `json:"fleet_id"`
	BossID  int64  `json:"fleet_boss_id"`
	Role    string `json:"role"`
	WingID  int64  `json:"wing_id"`
	SquadID int64  `json:"squad_id"`
}

// FleetSettings is the fleet-wide state: /fleets/{id}/.
type FleetSettings struct {
	MOTD           string `json:"motd"`
	IsFreeMove     bool   `json:"is_free_move"`
	IsRegistered   bool   `json:"is_registered"`
	IsVoiceEnabled bool   `json:"is_voice_enabled"`
}

// FleetMember is one line of /fleets/{id}/members/. StationID is 0 when
// the member is in space; structures never appear here.
type FleetMember struct {
	CharacterID    int64     `json:"character_id"`
	ShipTypeID     int64     `json:"ship_type_id"`
	SolarSystemID  int64     `json:"solar_system_id"`
	StationID      int64     `json:"station_id"`
	SquadID        int64     `json:"squad_id"`
	WingID         int64     `json:"wing_id"`
	Role           string    `json:"role"`
	RoleName       string    `json:"role_name"` // localized by ESI
	JoinTime       time.Time `json:"join_time"`
	TakesFleetWarp bool      `json:"takes_fleet_warp"`
}

type FleetSquad struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type FleetWing struct {
	ID     int64        `json:"id"`
	Name   string       `json:"name"`
	Squads []FleetSquad `json:"squads"`
}

// Fleet is the whole readable state, fetched in one go. Raw bodies are
// kept for the debug pane of the fleet page.
type Fleet struct {
	ID       int64
	Settings FleetSettings
	Members  []FleetMember
	Wings    []FleetWing

	RawSettings json.RawMessage
	RawMembers  json.RawMessage
	RawWings    json.RawMessage
}

// CharacterFleet reports the fleet a character is in. ok=false with a
// nil error means "not in a fleet" — ESI answers that with a 404.
func (c *Client) CharacterFleet(characterID int64) (*FleetRef, bool, error) {
	var ref FleetRef
	status, err := c.call("GET", characterID,
		fmt.Sprintf("/characters/%d/fleet/", characterID), nil, &ref)
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &ref, true, nil
}

// FleetOf reads settings, members and wings through the given character.
// Anyone but the fleet boss gets a 404 on every one of them.
func (c *Client) FleetOf(characterID, fleetID int64) (*Fleet, error) {
	f := &Fleet{ID: fleetID}
	base := fmt.Sprintf("/fleets/%d", fleetID)

	var wg sync.WaitGroup
	errs := make([]error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, errs[0] = c.call("GET", characterID, base+"/", nil, &f.RawSettings)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = c.call("GET", characterID, base+"/members/", nil, &f.RawMembers)
	}()
	go func() {
		defer wg.Done()
		_, errs[2] = c.call("GET", characterID, base+"/wings/", nil, &f.RawWings)
	}()
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(f.RawSettings, &f.Settings); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(f.RawMembers, &f.Members); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(f.RawWings, &f.Wings); err != nil {
		return nil, err
	}
	return f, nil
}

// FleetState reads just the fleet-wide settings — used to probe whether
// a character has access at all.
func (c *Client) FleetState(characterID, fleetID int64) (*FleetSettings, error) {
	var st FleetSettings
	_, err := c.call("GET", characterID, fmt.Sprintf("/fleets/%d/", fleetID), nil, &st)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// FleetUpdate changes the MOTD and/or the free-move flag; nil leaves the
// field alone (ESI treats an absent key as "unchanged").
func (c *Client) FleetUpdate(characterID, fleetID int64, motd *string, freeMove *bool) error {
	body := map[string]any{}
	if motd != nil {
		body["motd"] = *motd
	}
	if freeMove != nil {
		body["is_free_move"] = *freeMove
	}
	if len(body) == 0 {
		return nil
	}
	_, err := c.call("PUT", characterID, fmt.Sprintf("/fleets/%d/", fleetID), body, nil)
	return err
}

// placement builds the {role, wing_id, squad_id} body shared by invite
// and move. A fleet_commander takes neither wing nor squad.
func placement(role string, wingID, squadID int64) map[string]any {
	body := map[string]any{"role": role}
	if role != RoleFleetCommander {
		if wingID > 0 {
			body["wing_id"] = wingID
		}
		if squadID > 0 && role != RoleWingCommander {
			body["squad_id"] = squadID
		}
	}
	return body
}

// FleetInvite invites a character. Targets with a CSPA charge cannot be
// invited over ESI at all — that comes back as a 422.
func (c *Client) FleetInvite(characterID, fleetID, targetID int64, role string, wingID, squadID int64) error {
	body := placement(role, wingID, squadID)
	body["character_id"] = targetID
	_, err := c.call("POST", characterID, fmt.Sprintf("/fleets/%d/members/", fleetID), body, nil)
	return err
}

// FleetMove moves a member to another role / wing / squad.
func (c *Client) FleetMove(characterID, fleetID, memberID int64, role string, wingID, squadID int64) error {
	_, err := c.call("PUT", characterID,
		fmt.Sprintf("/fleets/%d/members/%d/", fleetID, memberID),
		placement(role, wingID, squadID), nil)
	return err
}

func (c *Client) FleetKick(characterID, fleetID, memberID int64) error {
	_, err := c.call("DELETE", characterID,
		fmt.Sprintf("/fleets/%d/members/%d/", fleetID, memberID), nil, nil)
	return err
}

func (c *Client) FleetCreateWing(characterID, fleetID int64) (int64, error) {
	var out struct {
		WingID int64 `json:"wing_id"`
	}
	_, err := c.call("POST", characterID, fmt.Sprintf("/fleets/%d/wings/", fleetID), nil, &out)
	return out.WingID, err
}

func (c *Client) FleetRenameWing(characterID, fleetID, wingID int64, name string) error {
	_, err := c.call("PUT", characterID,
		fmt.Sprintf("/fleets/%d/wings/%d/", fleetID, wingID),
		map[string]any{"name": name}, nil)
	return err
}

// FleetDeleteWing removes a wing; it must hold no members (empty squads
// inside are fine).
func (c *Client) FleetDeleteWing(characterID, fleetID, wingID int64) error {
	_, err := c.call("DELETE", characterID,
		fmt.Sprintf("/fleets/%d/wings/%d/", fleetID, wingID), nil, nil)
	return err
}

func (c *Client) FleetCreateSquad(characterID, fleetID, wingID int64) (int64, error) {
	var out struct {
		SquadID int64 `json:"squad_id"`
	}
	_, err := c.call("POST", characterID,
		fmt.Sprintf("/fleets/%d/wings/%d/squads/", fleetID, wingID), nil, &out)
	return out.SquadID, err
}

func (c *Client) FleetRenameSquad(characterID, fleetID, squadID int64, name string) error {
	_, err := c.call("PUT", characterID,
		fmt.Sprintf("/fleets/%d/squads/%d/", fleetID, squadID),
		map[string]any{"name": name}, nil)
	return err
}

// FleetDeleteSquad removes a squad; only empty ones can go.
func (c *Client) FleetDeleteSquad(characterID, fleetID, squadID int64) error {
	_, err := c.call("DELETE", characterID,
		fmt.Sprintf("/fleets/%d/squads/%d/", fleetID, squadID), nil, nil)
	return err
}
