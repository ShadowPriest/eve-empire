package esi

import "fmt"

// MarketOrderHistory returns the character's closed orders — cancelled,
// expired and fully filled. ESI keeps roughly 90 days of them
// (ПРОВЕРИТЬ), so accounting has to accumulate its own history.
//
// Why accounting needs it at all: the broker fee lives in the wallet
// journal keyed by ORDER id, and an order that has already closed is
// gone from /orders/. Without this endpoint the fee of every completed
// order would have nothing to attach to.
func (c *Client) MarketOrderHistory(characterID int64) ([]MarketOrder, error) {
	var out []MarketOrder
	for page := 1; ; page++ {
		var chunk []MarketOrder
		pages, err := c.get(characterID,
			fmt.Sprintf("/characters/%d/orders/history/?page=%d", characterID, page), &chunk)
		if err != nil {
			return out, err
		}
		out = append(out, chunk...)
		if page >= pages {
			return out, nil
		}
	}
}

// CorporationContracts is the corp counterpart of CharacterContracts
// (needs esi-contracts.read_corporation_contracts.v1 plus an in-game role).
func (c *Client) CorporationContracts(characterID, corporationID int64) ([]Contract, error) {
	var all []Contract
	for page := 1; ; page++ {
		var chunk []Contract
		pages, err := c.get(characterID,
			fmt.Sprintf("/corporations/%d/contracts/?page=%d", corporationID, page), &chunk)
		if err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if page >= pages {
			return all, nil
		}
	}
}

// CorporationContractItems lists a corp contract's cargo.
func (c *Client) CorporationContractItems(characterID, corporationID, contractID int64) ([]ContractItem, error) {
	var out []ContractItem
	_, err := c.get(characterID,
		fmt.Sprintf("/corporations/%d/contracts/%d/items/", corporationID, contractID), &out)
	return out, err
}
