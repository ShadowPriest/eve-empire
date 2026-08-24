// Package collect keeps a local copy of everything ESI forgets.
//
// It is stage 0 of ACCOUNTING.md and contains no accounting logic at all:
// raw ESI rows in, deduplicated rows out. The ledger is built on top later
// and can always be rebuilt from what is stored here — which is the whole
// point, because a purchase price that was never collected cannot be
// recovered from anywhere.
package collect

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/sched"
	"eve-empire/internal/store"
)

const (
	scopeWallet       = "esi-wallet.read_character_wallet.v1"
	scopeOrders       = "esi-markets.read_character_orders.v1"
	scopeJobs         = "esi-industry.read_character_jobs.v1"
	scopeAssets       = "esi-assets.read_assets.v1"
	scopeContracts    = "esi-contracts.read_character_contracts.v1"
	scopeCorpWallet   = "esi-wallet.read_corporation_wallets.v1"
	scopeCorpAssets   = "esi-assets.read_corporation_assets.v1"
	scopeCorpContract = "esi-contracts.read_corporation_contracts.v1"
)

type Collector struct {
	ESI   *esi.Client
	Store *store.Store
	// clientID is this copy's EVE application. A refresh token belongs to
	// the pair "application + character", so a token issued by the other
	// copy cannot be refreshed here — see internal/web/reauth.go.
	clientID string

	// warned remembers complaints so a missing permission or a dead token
	// is logged once per process instead of on every tick.
	warned map[string]bool
}

func New(e *esi.Client, st *store.Store, clientID string) *Collector {
	return &Collector{ESI: e, Store: st, clientID: clientID, warned: map[string]bool{}}
}

// chars returns the characters whose tokens this copy can actually
// refresh. Without the filter every tick would fire an SSO request per
// dead alt and get invalid_grant — noise in the log and load on CCP for
// a result that cannot change until the owner visits /reauth.
func (c *Collector) chars() ([]store.Character, error) {
	all, err := c.Store.Characters()
	if err != nil {
		return nil, err
	}
	clients, err := c.Store.TokenClients()
	if err != nil {
		return all, nil // no way to tell: better to try than to collect nothing
	}
	var out []store.Character
	skipped := 0
	for _, ch := range all {
		if clients[ch.ID] == c.clientID {
			out = append(out, ch)
			continue
		}
		skipped++
	}
	if skipped > 0 && !c.warned["reauth"] {
		c.warned["reauth"] = true
		log.Printf("сбор: %d альтов с чужими токенами пропущено — нужен перелогин на /reauth", skipped)
	}
	return out, nil
}

// Tasks are staggered: a restart must not fire every collector at once
// against the same ESI error budget.
//
// Intervals follow the measured ESI caches (ACCOUNTING.md §7.4): assets
// are cached exactly 3600 s server-side, so asking more often than hourly
// returns the identical snapshot; the contract LIST is cached 300 s, and
// contracts are what explain why goods left a hangar.
func (c *Collector) Tasks() []sched.Task {
	return []sched.Task{
		{Name: "contracts", Every: 5 * time.Minute, First: 15 * time.Second, Run: c.Contracts},
		{Name: "wallet", Every: time.Hour, First: 45 * time.Second, Run: c.Wallets},
		{Name: "orders", Every: time.Hour, First: 90 * time.Second, Run: c.Orders},
		{Name: "jobs", Every: 30 * time.Minute, First: 2 * time.Minute, Run: c.Jobs},
		{Name: "assets", Every: time.Hour, First: 3 * time.Minute, Run: c.Assets},
	}
}

// ── общее ────────────────────────────────────────────────────────────

// has reports whether the character's token carries the scope. Checking
// up front matters: a call without the scope is a 401, and 4xx answers
// count against the ESI error budget, so a permanently missing permission
// would burn it on every tick forever.
func (c *Collector) has(ch store.Character, scope string) bool {
	if slices.Contains(ch.Scopes, scope) {
		return true
	}
	key := scope + "/" + ch.Name
	if !c.warned[key] {
		c.warned[key] = true
		log.Printf("сбор: у %s нет права %s — пропускаю (нужен перелогин альта)", ch.Name, scope)
	}
	return false
}

// corpOf resolves and caches the corporation of each character for one
// run, so a fleet of alts in one corp is read once.
type corpRef struct {
	id      int64
	name    string
	viaChar store.Character
}

func (c *Collector) corps(chars []store.Character) []corpRef {
	seen := map[int64]bool{}
	var out []corpRef
	for _, ch := range chars {
		id, name, err := c.ESI.CharacterPublic(ch.ID)
		if err != nil || id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, corpRef{id: id, name: name, viaChar: ch})
	}
	return out
}

// note records the outcome of a run and returns a combined error.
func (c *Collector) note(task string, started time.Time, n int, errs []string) error {
	ok := len(errs) == 0
	msg := fmt.Sprintf("строк: %d", n)
	if !ok {
		msg = fmt.Sprintf("строк: %d; ошибки: %s", n, strings.Join(errs, "; "))
	}
	c.Store.MarkCollectorRun(task, started, ok, msg)
	if !ok {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ── кошелёк: транзакции и журнал ─────────────────────────────────────

func (c *Collector) Wallets(ctx context.Context) error {
	started := time.Now()
	chars, err := c.chars()
	if err != nil {
		return err
	}
	var errs []string
	total := 0

	for _, ch := range chars {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(ch, scopeWallet) {
			continue
		}
		if txs, err := c.ESI.WalletTransactions(ch.ID); err != nil {
			errs = append(errs, ch.Name+" транзакции: "+err.Error())
		} else {
			n, err := c.Store.SaveTransactions(txRows(ch.ID, 0, txs))
			if err != nil {
				errs = append(errs, ch.Name+" транзакции: "+err.Error())
			}
			total += n
		}
		if jr, err := c.ESI.WalletJournal(ch.ID); err != nil {
			errs = append(errs, ch.Name+" журнал: "+err.Error())
		} else {
			n, err := c.Store.SaveJournal(journalRows(ch.ID, 0, jr))
			if err != nil {
				errs = append(errs, ch.Name+" журнал: "+err.Error())
			}
			total += n
		}
	}

	// Corp wallets need the Accountant role in game; a character without
	// it simply yields nothing, which is not an error worth shouting about.
	for _, corp := range c.corps(chars) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(corp.viaChar, scopeCorpWallet) {
			continue
		}
		wallets, err := c.ESI.CorporationWallets(corp.viaChar.ID, corp.id)
		if err != nil {
			continue
		}
		for _, w := range wallets {
			if txs, err := c.ESI.CorporationWalletTransactions(corp.viaChar.ID, corp.id, w.Division); err == nil {
				n, _ := c.Store.SaveTransactions(txRows(corp.id, w.Division, txs))
				total += n
			}
			if jr, err := c.ESI.CorporationWalletJournal(corp.viaChar.ID, corp.id, w.Division); err == nil {
				n, _ := c.Store.SaveJournal(journalRows(corp.id, w.Division, jr))
				total += n
			}
		}
	}
	return c.note("wallet", started, total, errs)
}

func txRows(ownerID int64, division int, txs []esi.Transaction) []store.TxRow {
	out := make([]store.TxRow, 0, len(txs))
	for _, t := range txs {
		out = append(out, store.TxRow{
			OwnerID: ownerID, Division: division, TransactionID: t.TransactionID,
			JournalRefID: t.JournalRefID, ClientID: t.ClientID, IsBuy: t.IsBuy,
			IsPersonal: t.IsPersonal, Date: t.Date, TypeID: t.TypeID,
			Quantity: t.Quantity, UnitPrice: t.UnitPrice, LocationID: t.LocationID,
		})
	}
	return out
}

func journalRows(ownerID int64, division int, es []esi.JournalEntry) []store.JournalRow {
	out := make([]store.JournalRow, 0, len(es))
	for _, e := range es {
		out = append(out, store.JournalRow{
			OwnerID: ownerID, Division: division, ID: e.ID, Date: e.Date,
			RefType: e.RefType, Amount: e.Amount, Balance: e.Balance,
			ContextID: e.ContextID, ContextIDType: e.ContextIDType,
			FirstPartyID: e.FirstPartyID, SecondPartyID: e.SecondPartyID,
			Tax: e.Tax, TaxReceiverID: e.TaxReceiverID,
			Description: e.Description, Reason: e.Reason,
		})
	}
	return out
}

// ── ордера: живые и закрытые ─────────────────────────────────────────

// Orders collects both endpoints. The closed ones matter as much as the
// live ones: the broker fee sits in the journal keyed by ORDER id, and an
// order that has already filled is gone from /orders/.
func (c *Collector) Orders(ctx context.Context) error {
	started := time.Now()
	chars, err := c.chars()
	if err != nil {
		return err
	}
	var errs []string
	total := 0
	for _, ch := range chars {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(ch, scopeOrders) {
			continue
		}
		live, err := c.ESI.MarketOrders(ch.ID)
		if err != nil {
			errs = append(errs, ch.Name+" ордера: "+err.Error())
		} else {
			for i := range live {
				live[i].State = "open"
			}
			n, err := c.Store.SaveOrders(orderRows(ch.ID, live), started)
			if err != nil {
				errs = append(errs, ch.Name+" ордера: "+err.Error())
			}
			total += n
		}
		past, err := c.ESI.MarketOrderHistory(ch.ID)
		if err != nil {
			errs = append(errs, ch.Name+" история ордеров: "+err.Error())
			continue
		}
		n, err := c.Store.SaveOrders(orderRows(ch.ID, past), started)
		if err != nil {
			errs = append(errs, ch.Name+" история ордеров: "+err.Error())
		}
		total += n
	}
	return c.note("orders", started, total, errs)
}

func orderRows(ownerID int64, os []esi.MarketOrder) []store.OrderRow {
	out := make([]store.OrderRow, 0, len(os))
	for _, o := range os {
		state := o.State
		if state == "" {
			state = "open"
		}
		out = append(out, store.OrderRow{
			OwnerID: ownerID, OrderID: o.OrderID, TypeID: o.TypeID,
			IsBuy: o.IsBuyOrder, Price: o.Price, VolumeTotal: o.VolumeTotal,
			VolumeRemain: o.VolumeRemain, LocationID: o.LocationID,
			RegionID: o.RegionID, Issued: o.Issued, Duration: o.Duration,
			Escrow: o.Escrow, State: state,
		})
	}
	return out
}

// ── промышленные работы ──────────────────────────────────────────────

func (c *Collector) Jobs(ctx context.Context) error {
	started := time.Now()
	chars, err := c.chars()
	if err != nil {
		return err
	}
	var errs []string
	total := 0
	for _, ch := range chars {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(ch, scopeJobs) {
			continue
		}
		jobs, err := c.ESI.IndustryJobs(ch.ID)
		if err != nil {
			errs = append(errs, ch.Name+": "+err.Error())
			continue
		}
		rows := make([]store.JobRow, 0, len(jobs))
		for _, j := range jobs {
			rows = append(rows, store.JobRow{
				OwnerID: ch.ID, JobID: j.JobID, InstallerID: ch.ID,
				ActivityID: j.ActivityID, BlueprintID: j.BlueprintID,
				BlueprintTypeID: j.BlueprintType, ProductTypeID: j.ProductTypeID,
				Runs: j.Runs, SuccessfulRuns: j.SuccessfulRuns, Cost: j.Cost,
				Status: j.Status, FacilityID: j.FacilityID, StartDate: j.StartDate,
				EndDate: j.EndDate, CompletedDate: j.CompletedDate,
			})
		}
		n, err := c.Store.SaveJobs(rows, started)
		if err != nil {
			errs = append(errs, ch.Name+": "+err.Error())
		}
		total += n
	}
	return c.note("jobs", started, total, errs)
}

// ── контракты ────────────────────────────────────────────────────────

// Contracts runs every five minutes because the contract LIST is cached
// only 300 s and because contracts are the sole explanation for goods
// leaving a hangar without a sale.
//
// Cargo is fetched once per contract and never for couriers: ESI does not
// list a courier's cargo in any status (ПРОВЕРЕНО), so asking would waste
// a request per contract forever.
func (c *Collector) Contracts(ctx context.Context) error {
	started := time.Now()
	chars, err := c.chars()
	if err != nil {
		return err
	}
	var errs []string
	total := 0
	for _, ch := range chars {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(ch, scopeContracts) {
			continue
		}
		list, err := c.ESI.CharacterContracts(ch.ID)
		if err != nil {
			errs = append(errs, ch.Name+": "+err.Error())
			continue
		}
		n, err := c.Store.SaveContracts(contractRows(ch.ID, list), started)
		if err != nil {
			errs = append(errs, ch.Name+": "+err.Error())
			continue
		}
		total += n

		want, err := c.Store.ContractsNeedingItems(ch.ID, 20)
		if err != nil {
			continue
		}
		for _, id := range want {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			items, err := c.ESI.ContractItems(ch.ID, id)
			if err != nil {
				continue
			}
			rows := make([]store.ContractItemRow, 0, len(items))
			for _, it := range items {
				rows = append(rows, store.ContractItemRow{
					ContractID: id, RecordID: it.RecordID, TypeID: it.TypeID,
					Quantity: it.Quantity, RawQuantity: it.RawQuantity,
					IsIncluded: it.IsIncluded, IsSingleton: it.IsSingleton,
				})
			}
			// Mark as loaded even when empty, or an item_exchange with no
			// visible cargo would be re-requested every five minutes.
			c.Store.SaveContractItems(id, rows)
		}
	}
	return c.note("contracts", started, total, errs)
}

func contractRows(ownerID int64, cs []esi.Contract) []store.ContractRow {
	out := make([]store.ContractRow, 0, len(cs))
	for _, k := range cs {
		out = append(out, store.ContractRow{
			OwnerID: ownerID, ContractID: k.ContractID, Type: k.Type,
			Status: k.Status, Title: k.Title, ForCorporation: k.ForCorporation,
			IssuerID: k.IssuerID, IssuerCorpID: k.IssuerCorpID,
			AssigneeID: k.AssigneeID, AcceptorID: k.AcceptorID,
			StartLocationID: k.StartLocationID, EndLocationID: k.EndLocationID,
			DateIssued: k.DateIssued, DateAccepted: k.DateAccepted,
			DateCompleted: k.DateCompleted, Price: k.Price, Reward: k.Reward,
			Collateral: k.Collateral, Volume: k.Volume,
		})
	}
	return out
}

// ── имущество ────────────────────────────────────────────────────────

// Assets snapshots every character and corporation hourly and stores the
// DIFF. Hourly is not a guess: the ESI asset cache is exactly 3600 s and
// server-side, so a more frequent scan returns the identical snapshot.
func (c *Collector) Assets(ctx context.Context) error {
	started := time.Now()
	chars, err := c.chars()
	if err != nil {
		return err
	}
	var errs []string
	var sum store.AssetDiff

	add := func(d store.AssetDiff) {
		sum.Added += d.Added
		sum.Removed += d.Removed
		sum.Moved += d.Moved
		sum.Requantified += d.Requantified
	}

	for _, ch := range chars {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(ch, scopeAssets) {
			continue
		}
		raw, err := c.ESI.CharacterAssetRows(ch.ID)
		if err != nil {
			errs = append(errs, ch.Name+": "+err.Error())
			continue
		}
		rows := c.flatten(ch.ID, ch.ID, raw, func(ids []int64) (map[int64]string, error) {
			return c.ESI.AssetNames(ch.ID, ids)
		})
		d, err := c.Store.ApplyAssetSnapshot(ch.ID, rows, started)
		if err != nil {
			errs = append(errs, ch.Name+": "+err.Error())
			continue
		}
		add(d)
	}

	for _, corp := range c.corps(chars) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.has(corp.viaChar, scopeCorpAssets) {
			continue
		}
		raw, err := c.ESI.CorporationAssetRows(corp.viaChar.ID, corp.id)
		if err != nil {
			continue // needs the Director role; not every corp grants it
		}
		rows := c.flatten(corp.id, corp.viaChar.ID, raw, func(ids []int64) (map[int64]string, error) {
			return c.ESI.CorporationAssetNames(corp.viaChar.ID, corp.id, ids)
		})
		d, err := c.Store.ApplyAssetSnapshot(corp.id, rows, started)
		if err != nil {
			errs = append(errs, corp.name+": "+err.Error())
			continue
		}
		add(d)
	}

	msg := fmt.Sprintf("+%d −%d ↷%d ≠%d", sum.Added, sum.Removed, sum.Moved, sum.Requantified)
	if len(errs) > 0 {
		msg += "; ошибки: " + strings.Join(errs, "; ")
	}
	c.Store.MarkCollectorRun("assets", started, len(errs) == 0, msg)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// flatten resolves each row's immediate parent and its root station or
// structure.
//
// It deliberately does NOT reuse esi.groupAssets: that one collapses
// nesting up to the root on purpose, which is right for the assets page
// and useless here — accounting needs to know which container a stack
// sits in (ACCOUNTING.md §4).
//
// GRABLE: the contents of a freight container carry location_flag=AutoFit,
// not Cargo, so "is inside a container" can only be decided by the parent
// chain, never by the flag.
func (c *Collector) flatten(ownerID, viaChar int64, raw []esi.AssetRow,
	names func([]int64) (map[int64]string, error)) []store.AssetRow {

	byItem := make(map[int64]esi.AssetRow, len(raw))
	for _, a := range raw {
		byItem[a.ItemID] = a
	}
	rootOf := func(a esi.AssetRow) (root, parent int64) {
		loc := a.LocationID
		if _, nested := byItem[loc]; nested {
			parent = loc
		}
		for depth := 0; depth < 32; depth++ {
			p, ok := byItem[loc]
			if !ok {
				return loc, parent
			}
			loc = p.LocationID
		}
		return loc, parent
	}

	// Names are asked for only where they are still missing: they change
	// rarely, and the store keeps the previous one on update. A container
	// renamed in game therefore keeps its old label until its row is
	// rebuilt — acceptable, and far cheaper than re-asking hourly.
	known, _ := c.Store.NamedAssets(ownerID)
	var askFor []int64
	for _, a := range raw {
		if a.IsSingleton && !known[a.ItemID] {
			askFor = append(askFor, a.ItemID)
		}
	}
	fresh := map[int64]string{}
	if len(askFor) > 0 {
		if got, err := names(askFor); err == nil {
			fresh = got
		}
	}

	out := make([]store.AssetRow, 0, len(raw))
	for _, a := range raw {
		root, parent := rootOf(a)
		qty := a.Quantity
		if qty == 0 {
			qty = 1
		}
		out = append(out, store.AssetRow{
			OwnerID: ownerID, ItemID: a.ItemID, TypeID: a.TypeID, Quantity: qty,
			LocationID: a.LocationID, LocationFlag: a.LocationFlag,
			LocationType: a.LocationType, IsSingleton: a.IsSingleton,
			ParentItemID: parent, RootID: root, Name: fresh[a.ItemID],
		})
	}
	return out
}
