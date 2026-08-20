package esi

// Mining ledgers. This is the only place in ESI where extracted volume
// is reported directly — everything else (loot, salvage, reactions) has
// to be inferred from asset snapshots or the wallet.
//
// GRABLE: the corporation routes live under /corporation/ — singular,
// unlike every other corp route in ESI. It is CCP's typo, not ours.

import (
	"fmt"
	"sort"
	"time"
)

// MiningEntry is one line of the personal ledger: how much of one ore
// a character mined in one system on one day. ESI keeps 30 days.
type MiningEntry struct {
	Date          time.Time
	SolarSystemID int64
	TypeID        int64
	Quantity      int64
}

// MiningLedger returns the character's mining for the last 30 days,
// aggregated by (day, system, ore) exactly as ESI reports it.
func (c *Client) MiningLedger(characterID int64) ([]MiningEntry, error) {
	type row struct {
		Date          string `json:"date"`
		SolarSystemID int64  `json:"solar_system_id"`
		TypeID        int64  `json:"type_id"`
		Quantity      int64  `json:"quantity"`
	}
	var out []MiningEntry
	page := 1
	for {
		var chunk []row
		pages, err := c.get(characterID,
			fmt.Sprintf("/characters/%d/mining/?page=%d", characterID, page), &chunk)
		if err != nil {
			return nil, err
		}
		for _, r := range chunk {
			day, err := time.Parse("2006-01-02", r.Date)
			if err != nil {
				continue
			}
			out = append(out, MiningEntry{
				Date: day, SolarSystemID: r.SolarSystemID,
				TypeID: r.TypeID, Quantity: r.Quantity,
			})
		}
		if page >= pages || len(chunk) == 0 {
			return out, nil
		}
		page++
	}
}

// RegionTheForge is Jita's region: the price reference for everything.
const RegionTheForge = 10000002

// PriceDay is one day of market history.
type PriceDay struct {
	Day     time.Time
	Average float64
}

// PriceSeries is a type's daily prices, oldest first.
type PriceSeries []PriceDay

// At returns the price of the day, or the last known price before it.
// Market history lags a day or two behind, so the freshest entries are
// routinely missing and the previous day is the honest answer.
func (p PriceSeries) At(day time.Time) float64 {
	var out float64
	for _, d := range p {
		if d.Day.After(day) {
			break
		}
		out = d.Average
	}
	if out == 0 && len(p) > 0 {
		out = p[0].Average // mined before the series starts
	}
	return out
}

// JitaHistory returns ~13 months of daily average prices in The Forge.
// Public endpoint, cached by ESI until the daily rollover.
func (c *Client) JitaHistory(typeID int64) (PriceSeries, error) {
	return c.RegionHistory(RegionTheForge, typeID)
}

// RegionHistory is the same for any region — PLEX, for one, is not
// traded regionally at all and lives in its own global market.
func (c *Client) RegionHistory(regionID, typeID int64) (PriceSeries, error) {
	var raw []HistoryDay
	if _, err := c.get(0,
		fmt.Sprintf("/markets/%d/history/?type_id=%d", regionID, typeID), &raw); err != nil {
		return nil, err
	}
	out := make(PriceSeries, 0, len(raw))
	for _, d := range raw {
		day, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			continue
		}
		out = append(out, PriceDay{Day: day, Average: d.Average})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

// MiningObserver is a structure that records mining done at it —
// a refinery (moon mining) in practice.
type MiningObserver struct {
	ObserverID   int64  `json:"observer_id"`
	ObserverType string `json:"observer_type"`
	LastUpdated  string `json:"last_updated"`
}

// CorporationObservers lists the corporation's recording structures.
// Needs the Accountant or Director role.
func (c *Client) CorporationObservers(characterID, corporationID int64) ([]MiningObserver, error) {
	var out []MiningObserver
	page := 1
	for {
		var chunk []MiningObserver
		pages, err := c.get(characterID,
			fmt.Sprintf("/corporation/%d/mining/observers/?page=%d", corporationID, page), &chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if page >= pages || len(chunk) == 0 {
			return out, nil
		}
		page++
	}
}

// ObserverRecord is one pilot's take at one observer. Unlike the
// personal ledger this is per-character and has no system — the
// observer is the location.
type ObserverRecord struct {
	CharacterID           int64  `json:"character_id"`
	TypeID                int64  `json:"type_id"`
	Quantity              int64  `json:"quantity"`
	RecordedCorporationID int64  `json:"recorded_corporation_id"`
	LastUpdated           string `json:"last_updated"`
}

// ObserverLedger returns everything one observer has seen.
func (c *Client) ObserverLedger(characterID, corporationID, observerID int64) ([]ObserverRecord, error) {
	var out []ObserverRecord
	page := 1
	for {
		var chunk []ObserverRecord
		pages, err := c.get(characterID,
			fmt.Sprintf("/corporation/%d/mining/observers/%d/?page=%d",
				corporationID, observerID, page), &chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if page >= pages || len(chunk) == 0 {
			return out, nil
		}
		page++
	}
}

// MoonExtraction is one scheduled moon chunk.
type MoonExtraction struct {
	StructureID     int64     `json:"structure_id"`
	MoonID          int64     `json:"moon_id"`
	ExtractionStart time.Time `json:"extraction_start_time"`
	ChunkArrival    time.Time `json:"chunk_arrival_time"`
	NaturalDecay    time.Time `json:"natural_decay_time"`
}

// MoonName resolves a moon id. Moons (like planets and structures) are
// NOT resolvable through the /universe/names/ batch — putting one there
// fails the whole request.
func (c *Client) MoonName(moonID int64) string {
	var m struct {
		Name string `json:"name"`
	}
	if _, err := c.get(0, fmt.Sprintf("/universe/moons/%d/", moonID), &m); err == nil && m.Name != "" {
		return m.Name
	}
	return fmt.Sprintf("Луна %d", moonID)
}

// MoonExtractions lists the corporation's moon extraction timers.
func (c *Client) MoonExtractions(characterID, corporationID int64) ([]MoonExtraction, error) {
	var out []MoonExtraction
	_, err := c.get(characterID,
		fmt.Sprintf("/corporation/%d/mining/extractions/", corporationID), &out)
	return out, err
}
