package sde

// The market group tree — the same hierarchy the in-game market browser
// shows. Built once in memory: 2106 groups, cheap to keep. Subtrees
// that contain no published tradeable type at any depth are pruned —
// they would render as dead folders.

import (
	"database/sql"
	"sort"
	"sync"
)

// MarketGroupNode is one folder of the market tree.
type MarketGroupNode struct {
	ID       int64
	Name     string // localized (ru preferred)
	HasTypes bool   // has published types of its own
	Children []*MarketGroupNode
}

var marketGroups struct {
	once   sync.Once
	roots  []*MarketGroupNode
	byID   map[int64]*MarketGroupNode
	parent map[int64]int64
}

// MarketGroups returns the roots of the market tree.
func (d *DB) MarketGroups() []*MarketGroupNode {
	d.loadMarketGroups()
	return marketGroups.roots
}

// MarketGroupName returns the localized name of one group.
func (d *DB) MarketGroupName(id int64) string {
	d.loadMarketGroups()
	if n := marketGroups.byID[id]; n != nil {
		return n.Name
	}
	return ""
}

// MarketGroupPath returns the ids from the root down to the given
// group, the group itself included — the tree opens along it.
func (d *DB) MarketGroupPath(id int64) []int64 {
	d.loadMarketGroups()
	var path []int64
	for id != 0 {
		if _, ok := marketGroups.byID[id]; !ok {
			break
		}
		path = append([]int64{id}, path...)
		id = marketGroups.parent[id]
	}
	return path
}

// MarketGroupTypes lists the published types sitting directly in one
// group, sorted by localized name.
func (d *DB) MarketGroupTypes(groupID int64) []MarketType {
	if !d.Available() {
		return nil
	}
	rows, err := d.db.Query(`SELECT type_id, name_en, name_ru FROM types
		WHERE published = 1 AND market_group_id = ?`, groupID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []MarketType
	for rows.Next() {
		var (
			id     int64
			en, ru sql.NullString
		)
		if rows.Scan(&id, &en, &ru) == nil {
			out = append(out, MarketType{TypeID: id, Name: pick(ru, en), NameEn: en.String})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TypeMarketGroup returns the market group a type sits in (0 if none) —
// picking a type from the search opens the tree at its folder.
func (d *DB) TypeMarketGroup(typeID int64) int64 {
	if !d.Available() {
		return 0
	}
	var id sql.NullInt64
	d.db.QueryRow(`SELECT market_group_id FROM types WHERE type_id = ?`, typeID).Scan(&id)
	return id.Int64
}

func (d *DB) loadMarketGroups() {
	marketGroups.once.Do(func() {
		marketGroups.byID = map[int64]*MarketGroupNode{}
		marketGroups.parent = map[int64]int64{}
		if !d.Available() {
			return
		}
		rows, err := d.db.Query(`SELECT market_group_id, parent_id, name_en, name_ru
			FROM market_groups`)
		if err != nil {
			return
		}
		for rows.Next() {
			var (
				id, parent int64
				en, ru     sql.NullString
			)
			if rows.Scan(&id, &parent, &en, &ru) != nil {
				continue
			}
			marketGroups.byID[id] = &MarketGroupNode{ID: id, Name: pick(ru, en)}
			marketGroups.parent[id] = parent
		}
		rows.Close()

		// Which groups hold published types of their own.
		if trows, err := d.db.Query(`SELECT market_group_id, COUNT(*) FROM types
			WHERE published = 1 AND market_group_id IS NOT NULL
			GROUP BY market_group_id`); err == nil {
			for trows.Next() {
				var id, n int64
				if trows.Scan(&id, &n) == nil && n > 0 {
					if node := marketGroups.byID[id]; node != nil {
						node.HasTypes = true
					}
				}
			}
			trows.Close()
		}

		for id, node := range marketGroups.byID {
			if p := marketGroups.byID[marketGroups.parent[id]]; p != nil {
				p.Children = append(p.Children, node)
			} else {
				marketGroups.roots = append(marketGroups.roots, node)
			}
		}

		// Prune folders that are empty all the way down.
		var alive func(n *MarketGroupNode) bool
		alive = func(n *MarketGroupNode) bool {
			kept := n.Children[:0]
			for _, c := range n.Children {
				if alive(c) {
					kept = append(kept, c)
				}
			}
			n.Children = kept
			return n.HasTypes || len(n.Children) > 0
		}
		keptRoots := marketGroups.roots[:0]
		for _, r := range marketGroups.roots {
			if alive(r) {
				keptRoots = append(keptRoots, r)
			}
		}
		marketGroups.roots = keptRoots

		var sortTree func(list []*MarketGroupNode)
		sortTree = func(list []*MarketGroupNode) {
			sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
			for _, n := range list {
				sortTree(n.Children)
			}
		}
		sortTree(marketGroups.roots)
	})
}
