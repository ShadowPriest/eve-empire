package esi

import "fmt"

// AssetRow is one raw /assets/ line with the fields groupAssets throws
// away. Accounting needs them: location_flag tells a hangar from a
// container from a ship's cargo, and only assembled (singleton) items
// can carry a name.
type AssetRow struct {
	ItemID          int64  `json:"item_id"`
	TypeID          int64  `json:"type_id"`
	LocationID      int64  `json:"location_id"`
	LocationFlag    string `json:"location_flag"`
	LocationType    string `json:"location_type"` // station|solar_system|item|other
	Quantity        int64  `json:"quantity"`
	IsSingleton     bool   `json:"is_singleton"`
	IsBlueprintCopy bool   `json:"is_blueprint_copy"`
}

func (c *Client) assetRowPages(characterID int64, pathFmt string) ([]AssetRow, error) {
	var all []AssetRow
	for page := 1; ; page++ {
		var chunk []AssetRow
		pages, err := c.get(characterID, fmt.Sprintf(pathFmt, page), &chunk)
		if err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if page >= pages {
			return all, nil
		}
	}
}

// CharacterAssetRows returns the character's assets as ESI reports
// them, flat and with the parent chain intact.
func (c *Client) CharacterAssetRows(characterID int64) ([]AssetRow, error) {
	return c.assetRowPages(characterID, fmt.Sprintf("/characters/%d/assets/?page=%%d", characterID))
}

// CorporationAssetRows is the corp equivalent (needs the Director role).
func (c *Client) CorporationAssetRows(characterID, corporationID int64) ([]AssetRow, error) {
	return c.assetRowPages(characterID, fmt.Sprintf("/corporations/%d/assets/?page=%%d", corporationID))
}

// assetNames resolves item names for assembled items (ships, containers).
//
// GRABLE 1: the route is a POST, but it is a READ — the body is just the
// id list. ESI has no rename endpoint at all.
//
// GRABLE 2: one un-nameable id kills the WHOLE batch with 404 "Invalid
// IDs in the request", exactly like /universe/names/. Blueprints are the
// usual culprit: is_singleton is true for them, but they cannot carry a
// name. Rather than guess the nameable types, bisect a failed batch and
// drop the ids that fail alone.
func (c *Client) assetNames(characterID int64, path string, itemIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	ids := dedupe(itemIDs)
	for start := 0; start < len(ids); start += 1000 {
		c.assetNameBatch(characterID, path, ids[start:min(start+1000, len(ids))], out)
	}
	return out, nil
}

// assetNameBatch fills out with whatever names it can get, halving the
// batch on failure. A single id that fails on its own is un-nameable and
// is simply skipped.
func (c *Client) assetNameBatch(characterID int64, path string, ids []int64, out map[int64]string) {
	if len(ids) == 0 {
		return
	}
	var chunk []struct {
		ItemID int64  `json:"item_id"`
		Name   string `json:"name"`
	}
	if _, err := c.call("POST", characterID, path, ids, &chunk); err != nil {
		if len(ids) == 1 {
			return
		}
		half := len(ids) / 2
		c.assetNameBatch(characterID, path, ids[:half], out)
		c.assetNameBatch(characterID, path, ids[half:], out)
		return
	}
	for _, n := range chunk {
		if n.Name != "" && n.Name != "None" {
			out[n.ItemID] = n.Name
		}
	}
}

// AssetNames resolves the character's own item names.
func (c *Client) AssetNames(characterID int64, itemIDs []int64) (map[int64]string, error) {
	return c.assetNames(characterID, fmt.Sprintf("/characters/%d/assets/names/", characterID), itemIDs)
}

// CorporationAssetNames resolves corp item names (needs the Director role).
func (c *Client) CorporationAssetNames(characterID, corporationID int64, itemIDs []int64) (map[int64]string, error) {
	return c.assetNames(characterID, fmt.Sprintf("/corporations/%d/assets/names/", corporationID), itemIDs)
}
