package sde

import (
	"database/sql"
	"strings"
	"sync"
)

// Dogma ids behind the skill catalog. Note the two different families:
// 175..179 are the IMPLANT bonus attributes (see AttrByName above), while
// 164..168 are the CHARACTER attributes a skill trains on — the values
// stored in attributes 180/181.
const (
	AttrPrimaryAttribute   = 180
	AttrSecondaryAttribute = 181
	AttrSkillTimeConstant  = 275 // "rank"

	skillCategory = 16
)

// charAttrByID maps the value found in attributes 180/181 to our key.
var charAttrByID = map[int64]string{
	164: "charisma",
	165: "intelligence",
	166: "memory",
	167: "perception",
	168: "willpower",
}

// SkillInfo is one skill from the catalog: what a level costs (Rank),
// which attributes it trains on and what it needs first.
type SkillInfo struct {
	ID     int64
	NameEn string
	NameRu string
	Rank   int
	Prim   string // character attribute key, "" when the SDE has none
	Sec    string
	Pre    map[int64]int // prerequisite skill id -> level
}

// Name returns the localized name, falling back to English.
func (s SkillInfo) Name() string {
	if s.NameRu != "" {
		return s.NameRu
	}
	return s.NameEn
}

type skillCatalog struct {
	once   sync.Once
	byID   map[int64]SkillInfo
	byName map[string]int64 // lowercased en and ru -> id
}

var catalog skillCatalog

// Skills returns the whole skill catalog keyed by type id. The catalog is
// small (a few thousand rows) and never changes, so it is read once and
// kept in memory. A missing sde.db yields an empty map.
func (d *DB) Skills() map[int64]SkillInfo {
	d.loadSkills()
	return catalog.byID
}

// SkillByName resolves a skill by its English or localized name,
// case-insensitively. Used by the plan importer, which sees whatever the
// game put on the clipboard.
func (d *DB) SkillByName(name string) (SkillInfo, bool) {
	d.loadSkills()
	id, ok := catalog.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return SkillInfo{}, false
	}
	return catalog.byID[id], true
}

func (d *DB) loadSkills() {
	catalog.once.Do(func() {
		catalog.byID = map[int64]SkillInfo{}
		catalog.byName = map[string]int64{}
		if !d.Available() {
			return
		}
		rows, err := d.db.Query(`SELECT t.type_id, t.name_en, t.name_ru
			FROM types t JOIN groups g ON g.group_id = t.group_id
			WHERE g.category_id = ? AND t.published = 1`, skillCategory)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id            int64
				en, ru        sql.NullString
			)
			if rows.Scan(&id, &en, &ru) != nil {
				continue
			}
			catalog.byID[id] = SkillInfo{
				ID: id, NameEn: en.String, NameRu: ru.String, Pre: map[int64]int{},
			}
		}
		// ranks and training attributes
		arows, err := d.db.Query(`SELECT type_id, attribute_id, value FROM type_attributes
			WHERE attribute_id IN (?,?,?)`,
			AttrSkillTimeConstant, AttrPrimaryAttribute, AttrSecondaryAttribute)
		if err == nil {
			for arows.Next() {
				var id, attr int64
				var val float64
				if arows.Scan(&id, &attr, &val) != nil {
					continue
				}
				sk, ok := catalog.byID[id]
				if !ok {
					continue
				}
				switch attr {
				case AttrSkillTimeConstant:
					sk.Rank = int(val)
				case AttrPrimaryAttribute:
					sk.Prim = charAttrByID[int64(val)]
				case AttrSecondaryAttribute:
					sk.Sec = charAttrByID[int64(val)]
				}
				catalog.byID[id] = sk
			}
			arows.Close()
		}
		// prerequisites
		prows, err := d.db.Query(`SELECT type_id, skill_type_id, level FROM type_skills`)
		if err == nil {
			for prows.Next() {
				var id, pre int64
				var lv int
				if prows.Scan(&id, &pre, &lv) != nil {
					continue
				}
				if sk, ok := catalog.byID[id]; ok {
					sk.Pre[pre] = lv
				}
			}
			prows.Close()
		}
		for id, sk := range catalog.byID {
			if sk.NameEn != "" {
				catalog.byName[strings.ToLower(sk.NameEn)] = id
			}
			if sk.NameRu != "" {
				catalog.byName[strings.ToLower(sk.NameRu)] = id
			}
		}
	})
}
