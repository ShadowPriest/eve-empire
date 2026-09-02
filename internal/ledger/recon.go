package ledger

import (
	"fmt"
	"sort"
	"time"

	"eve-empire/internal/store"
)

// Сверка: превращение расхождений в документы (ACCOUNTING.md §7.3).
//
// Классификация намеренно осторожная. Пара «столько же убыло здесь,
// столько же прибыло там» — это перемещение, и его можно проводить
// автоматически: себестоимость просто переезжает, итог не меняется.
// Всё остальное трогает деньги и остаётся решением человека.

type Proposal struct {
	Kind   string // transfer | receipt | writeoff
	TypeID int64
	Qty    int64

	FromOwner, FromLocation int64
	ToOwner, ToLocation     int64

	// Пометки для человека: почему строка попала сюда.
	SameOwner bool
	OnMarket  int64
	InSafety  int64
}

// Classify pairs shortages with surpluses of the same type. What pairs is
// a movement; what does not is a real gain or loss and needs a decision.
//
// Greedy by size: the biggest shortage takes the biggest surplus. With one
// or two piles of a type — the normal case — this is exact; with many it
// is a proposal, which is why nothing here posts by itself.
func Classify(sum store.ReconSummary) []Proposal {
	byType := map[int64][]store.ReconLine{}
	for _, l := range sum.Lines {
		byType[l.TypeID] = append(byType[l.TypeID], l)
	}
	types := make([]int64, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	var out []Proposal
	for _, t := range types {
		var short, extra []store.ReconLine
		for _, l := range byType[t] {
			if l.Diff < 0 {
				short = append(short, l)
			} else {
				extra = append(extra, l)
			}
		}
		sort.Slice(short, func(i, j int) bool { return short[i].Diff < short[j].Diff })
		sort.Slice(extra, func(i, j int) bool { return extra[i].Diff > extra[j].Diff })

		si, ei := 0, 0
		need := int64(0)
		have := int64(0)
		for si < len(short) || ei < len(extra) {
			if need == 0 && si < len(short) {
				need = -short[si].Diff
			}
			if have == 0 && ei < len(extra) {
				have = extra[ei].Diff
			}
			switch {
			case need > 0 && have > 0:
				qty := need
				if have < qty {
					qty = have
				}
				out = append(out, Proposal{
					Kind: "transfer", TypeID: t, Qty: qty,
					FromOwner: short[si].OwnerID, FromLocation: short[si].LocationID,
					ToOwner: extra[ei].OwnerID, ToLocation: extra[ei].LocationID,
					SameOwner: short[si].OwnerID == extra[ei].OwnerID,
					OnMarket:  extra[ei].OnMarket, InSafety: extra[ei].InSafety,
				})
				need -= qty
				have -= qty
				if need == 0 {
					si++
				}
				if have == 0 {
					ei++
				}
			case need > 0:
				out = append(out, Proposal{
					Kind: "writeoff", TypeID: t, Qty: need,
					FromOwner: short[si].OwnerID, FromLocation: short[si].LocationID,
				})
				need = 0
				si++
			case have > 0:
				out = append(out, Proposal{
					Kind: "receipt", TypeID: t, Qty: have,
					ToOwner: extra[ei].OwnerID, ToLocation: extra[ei].LocationID,
					OnMarket: extra[ei].OnMarket, InSafety: extra[ei].InSafety,
				})
				have = 0
				ei++
			default:
				si, ei = len(short), len(extra)
			}
		}
	}
	return out
}

// manualID makes a unique src_id for a document a human caused. Time in
// nanoseconds is both unique and useful when reading the ledger later.
func manualID(prefix string) string {
	return fmt.Sprintf("%s:%d", prefix, time.Now().UnixNano())
}

// PostTransfer moves goods between places at cost: the receipt takes
// exactly what the issue took off the shelf, so shuffling stock around
// can never create or destroy profit.
func (b *Builder) PostTransfer(p Proposal) error {
	at := time.Now()
	from := store.PlaceKey{OwnerID: p.FromOwner, LocationID: p.FromLocation, Flag: "Hangar"}
	to := store.PlaceKey{OwnerID: p.ToOwner, LocationID: p.ToLocation, Flag: "Hangar"}
	_, err := b.Store.PostDoc(store.Doc{
		Kind: "transfer", OwnerID: p.FromOwner, At: at,
		Src: "manual", SrcID: manualID("transfer"),
		Note: "перемещение по сверке",
	}, []store.Line{
		{Place: from, TypeID: p.TypeID, Qty: -p.Qty, Scope: "location"},
		{Place: to, TypeID: p.TypeID, Qty: p.Qty, CostFrom: "issue"},
	}, nil)
	return err
}

// PostReceipt is the manual incoming document of ACCOUNTING.md §6: goods
// appeared and only a human knows where from.
//
// A zero cost is a legitimate answer — mined ore and loot really did cost
// nothing — but it is recorded as a fact, not as a guess, so the two never
// blur together in a report.
func (b *Builder) PostReceipt(owner, location, typeID, qty int64,
	unitCost float64, source string, estimated bool) error {

	kind := "fact"
	if estimated {
		kind = "estimate"
	}
	at := time.Now()
	_, err := b.Store.PostDoc(store.Doc{
		Kind: "receipt", OwnerID: owner, At: at,
		Src: "manual", SrcID: manualID("receipt"),
		Note: "приход: " + source,
	}, []store.Line{{
		Place:     store.PlaceKey{OwnerID: owner, LocationID: location, Flag: "Hangar"},
		TypeID:    typeID,
		Qty:       qty,
		CostTotal: unitCost * float64(qty),
		MktTotal:  unitCost * float64(qty),
		CostKind:  kind,
	}}, nil)
	return err
}

// PostWriteOff removes goods the ledger still believes in. The cost goes
// to loss, which is the whole difference between a write-off and a sale.
func (b *Builder) PostWriteOff(owner, location, typeID, qty int64, reason string) error {
	at := time.Now()
	_, err := b.Store.PostDoc(store.Doc{
		Kind: "writeoff", OwnerID: owner, At: at,
		Src: "manual", SrcID: manualID("writeoff"),
		Note: "списание: " + reason,
	}, []store.Line{{
		Place:  store.PlaceKey{OwnerID: owner, LocationID: location, Flag: "Hangar"},
		TypeID: typeID, Qty: -qty, Scope: "location",
	}}, nil)
	return err
}

// ApplyTransfers posts every movement the classifier is sure about and
// reports how many. Surpluses and shortages are deliberately left alone:
// they change the money, and that is a human's call.
func (b *Builder) ApplyTransfers(props []Proposal) (int, error) {
	n := 0
	for _, p := range props {
		if p.Kind != "transfer" {
			continue
		}
		if err := b.PostTransfer(p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
