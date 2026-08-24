package esi

import (
	"fmt"
	"time"
)

// Contract is one entry of /characters/{id}/contracts/. Courier contracts
// are how goods legitimately leave a hangar without being sold, so for
// accounting they are movement documents, not trades.
type Contract struct {
	ContractID      int64     `json:"contract_id"`
	Type            string    `json:"type"` // item_exchange|courier|auction|loan
	Status          string    `json:"status"`
	Title           string    `json:"title"`
	ForCorporation  bool      `json:"for_corporation"`
	IssuerID        int64     `json:"issuer_id"`
	IssuerCorpID    int64     `json:"issuer_corporation_id"`
	AssigneeID      int64     `json:"assignee_id"`
	AcceptorID      int64     `json:"acceptor_id"`
	StartLocationID int64     `json:"start_location_id"`
	EndLocationID   int64     `json:"end_location_id"`
	DateIssued      time.Time `json:"date_issued"`
	DateExpired     time.Time `json:"date_expired"`
	DateAccepted    time.Time `json:"date_accepted"`
	DateCompleted   time.Time `json:"date_completed"`
	Price           float64   `json:"price"`
	Reward          float64   `json:"reward"`
	Collateral      float64   `json:"collateral"`
	Volume          float64   `json:"volume"`
	DaysToComplete  int       `json:"days_to_complete"`
}

// ContractItem is one line of a contract's cargo.
//
// GRABLE: the line carries record_id, NOT item_id — a contract does not
// tell you which physical stack moved, only what and how much. Tying the
// cargo back to a lot has to go through the asset diff.
type ContractItem struct {
	RecordID    int64 `json:"record_id"`
	TypeID      int64 `json:"type_id"`
	Quantity    int64 `json:"quantity"`
	RawQuantity int64 `json:"raw_quantity"`
	IsIncluded  bool  `json:"is_included"`
	IsSingleton bool  `json:"is_singleton"`
}

// CharacterContracts returns the character's contracts (ESI keeps
// finished ones for about 30 days, so history must be accumulated).
func (c *Client) CharacterContracts(characterID int64) ([]Contract, error) {
	var all []Contract
	for page := 1; ; page++ {
		var chunk []Contract
		pages, err := c.get(characterID, fmt.Sprintf("/characters/%d/contracts/?page=%d", characterID, page), &chunk)
		if err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if page >= pages {
			return all, nil
		}
	}
}

// ContractItems lists a contract's cargo. Only available while the
// contract is visible to the character.
func (c *Client) ContractItems(characterID, contractID int64) ([]ContractItem, error) {
	var out []ContractItem
	_, err := c.get(characterID, fmt.Sprintf("/characters/%d/contracts/%d/items/", characterID, contractID), &out)
	return out, err
}
