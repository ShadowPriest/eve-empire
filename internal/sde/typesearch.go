package sde

// Type search for the orders tool: any published type with a market
// group is tradeable. Held in memory for the same reason the recipe
// index is — SQLite LIKE is case-insensitive only for ASCII, and the
// owner types Russian names.

import (
	"database/sql"
	"sort"
	"strings"
	"sync"
)

// MarketType is one tradeable type as the search returns it.
type MarketType struct {
	TypeID int64
	Name   string // localized (ru preferred)
	NameEn string
}

var marketTypes struct {
	once sync.Once
	all  []MarketType
	hay  []string // lowercased "name_en name_ru", same index as all
}

// SearchMarketTypes finds tradeable types by name in either language.
// Exact hits come first, then prefixes, then substrings; ties go to the
// shorter name so "Tritanium" beats "Compressed Tritanium".
func (d *DB) SearchMarketTypes(q string, limit int) []MarketType {
	d.loadMarketTypes()
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}

	type scored struct {
		mt    MarketType
		score int
	}
	var hits []scored
	for i, hay := range marketTypes.hay {
		if !strings.Contains(hay, q) {
			continue
		}
		mt := marketTypes.all[i]
		score := 3
		switch {
		case strings.EqualFold(mt.Name, q) || strings.EqualFold(mt.NameEn, q):
			score = 0
		case strings.HasPrefix(strings.ToLower(mt.Name), q) ||
			strings.HasPrefix(strings.ToLower(mt.NameEn), q):
			score = 1
		case strings.Contains(strings.ToLower(mt.Name), q):
			score = 2
		}
		hits = append(hits, scored{mt, score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score < hits[j].score
		}
		if len(hits[i].mt.Name) != len(hits[j].mt.Name) {
			return len(hits[i].mt.Name) < len(hits[j].mt.Name)
		}
		return hits[i].mt.Name < hits[j].mt.Name
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]MarketType, len(hits))
	for i, h := range hits {
		out[i] = h.mt
	}
	return out
}

func (d *DB) loadMarketTypes() {
	marketTypes.once.Do(func() {
		if !d.Available() {
			return
		}
		rows, err := d.db.Query(`SELECT type_id, name_en, name_ru FROM types
			WHERE published = 1 AND market_group_id IS NOT NULL`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id     int64
				en, ru sql.NullString
			)
			if rows.Scan(&id, &en, &ru) != nil {
				continue
			}
			marketTypes.all = append(marketTypes.all, MarketType{
				TypeID: id, Name: pick(ru, en), NameEn: en.String,
			})
			marketTypes.hay = append(marketTypes.hay,
				strings.ToLower(en.String+" "+ru.String))
		}
	})
}
