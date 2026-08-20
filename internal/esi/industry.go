package esi

// Industry cost indices — the system half of an installation fee.
//
// The job fee is charged on the ESTIMATED ITEM VALUE (adjusted prices of
// the blueprint's base materials, before ME), not on what the materials
// cost on the market:
//
//	fee = EIV × (system index + facility tax + SCC surcharge)
//
// ESI hands out the system index for free (public route, cached an hour);
// the tax is a property of the structure and the surcharge is a flat
// game-wide number, so both stay editable on the page.

import "fmt"

// SCCSurcharge is the flat share of the estimated item value CCP adds to
// every industry job on top of the system index and the facility tax.
const SCCSurcharge = 1.5

// CostIndices holds one system's cost index per activity, in per cent of
// the estimated item value.
type CostIndices struct {
	Manufacturing float64
	Reaction      float64
	Copying       float64
	Invention     float64
	ResearchME    float64
	ResearchTE    float64
}

// For returns the index of a blueprint activity ("manufacturing" or
// "reaction" — the two the calculator builds with).
func (c CostIndices) For(activity string) float64 {
	if activity == "reaction" {
		return c.Reaction
	}
	return c.Manufacturing
}

type systemCost struct {
	SystemID int64 `json:"solar_system_id"`
	Indices  []struct {
		Activity string  `json:"activity"`
		Index    float64 `json:"cost_index"`
	} `json:"cost_indices"`
}

// IndustrySystems returns the cost indices of every system, keyed by
// system id. Public route; one call covers all ~5000 systems.
func (c *Client) IndustrySystems() (map[int64]CostIndices, error) {
	var list []systemCost
	if _, err := c.get(0, "/industry/systems/", &list); err != nil {
		return nil, err
	}
	out := make(map[int64]CostIndices, len(list))
	for _, s := range list {
		var ci CostIndices
		for _, i := range s.Indices {
			// Stored as a fraction (0.0195), shown as per cent.
			v := i.Index * 100
			switch i.Activity {
			case "manufacturing":
				ci.Manufacturing = v
			case "reaction":
				ci.Reaction = v
			case "copying":
				ci.Copying = v
			case "invention":
				ci.Invention = v
			case "researching_material_efficiency":
				ci.ResearchME = v
			case "researching_time_efficiency":
				ci.ResearchTE = v
			}
		}
		out[s.SystemID] = ci
	}
	return out, nil
}

// SystemIndex resolves a system by name and returns its cost indices.
func (c *Client) SystemIndex(name string) (int64, CostIndices, error) {
	id, err := c.ResolveSystem(name)
	if err != nil {
		return 0, CostIndices{}, err
	}
	all, err := c.IndustrySystems()
	if err != nil {
		return id, CostIndices{}, err
	}
	ci, ok := all[id]
	if !ok {
		return id, CostIndices{}, fmt.Errorf("для системы %q индексов в ESI нет", name)
	}
	return id, ci, nil
}
