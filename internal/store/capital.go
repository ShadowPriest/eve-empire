package store

import "time"

// Капитал и контрольная сумма (ACCOUNTING.md §10.6).
//
// Тождество простое: ISK на кошельках плюс склад по себестоимости — это
// весь капитал. Изменение капитала за период обязано равняться прибыли.
//
// На практике оно НЕ сойдётся, и это не дефект, а смысл отчёта. Реестр
// знает только рыночные сделки; баунти, награды миссий, продажа PLEX,
// переводы между своими и всё прочее движение ISK в него не попадает.
// Поэтому отчёт не выносит вердикт «сошлось/не сошлось», а показывает
// НЕВЯЗКУ и раскладывает её по ref_type журнала — так сразу видно, чего
// именно не хватает, вместо бесполезного «где-то ошибка».

// CashKind is one journal ref_type over the period.
type CashKind struct {
	RefType string
	Amount  float64
	Posted  bool // реестр про такие движения знает
}

// CapitalCheck is the whole balance identity for one period.
type CapitalCheck struct {
	From, To time.Time

	ISKFrom, ISKTo     float64
	StockFrom, StockTo float64
	Escrow             float64 // ISK под своими ордерами на покупку, на конец

	DeltaISK   float64
	DeltaStock float64

	Opening float64 // начальные остатки, попавшие в период: это вклад
	// собственника, а не заработок
	Explained float64 // движение ISK, которое реестр провёл (acc_cash)
	Residual  float64 // всё остальное: DeltaISK − Explained
	Profit    float64 // Explained + DeltaStock

	Kinds []CashKind
}

// postedRefTypes are the journal lines the ledger already turns into
// documents. Everything else lands in the residual.
var postedRefTypes = map[string]bool{
	"market_transaction": true,
	"transaction_tax":    true,
	"brokers_fee":        true,
}

// Capital computes the identity between two instants.
func (s *Store) Capital(from, to time.Time) (CapitalCheck, error) {
	c := CapitalCheck{From: from, To: to}

	var err error
	if c.ISKFrom, err = s.iskAt(from); err != nil {
		return c, err
	}
	if c.ISKTo, err = s.iskAt(to); err != nil {
		return c, err
	}
	if c.StockFrom, err = s.stockAt(from); err != nil {
		return c, err
	}
	if c.StockTo, err = s.stockAt(to); err != nil {
		return c, err
	}
	if err = s.db.QueryRow(`SELECT COALESCE(SUM(escrow),0) FROM hist_order
		WHERE is_buy = 1 AND state = 'open'`).Scan(&c.Escrow); err != nil {
		return c, err
	}
	if err = s.db.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM acc_cash
		WHERE at >= ? AND at < ?`, from.Unix(), to.Unix()).Scan(&c.Explained); err != nil {
		return c, err
	}

	rows, err := s.db.Query(`SELECT ref_type, SUM(amount) FROM hist_journal
		WHERE date >= ? AND date < ?
		GROUP BY ref_type ORDER BY ABS(SUM(amount)) DESC`, from.Unix(), to.Unix())
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var k CashKind
		if err := rows.Scan(&k.RefType, &k.Amount); err != nil {
			return c, err
		}
		k.Posted = postedRefTypes[k.RefType]
		c.Kinds = append(c.Kinds, k)
	}
	if err := rows.Err(); err != nil {
		return c, err
	}

	if err = s.db.QueryRow(`SELECT COALESCE(SUM(m.cost),0)
		FROM acc_move m JOIN acc_doc d ON d.id = m.doc_id
		WHERE d.kind = 'opening' AND m.at >= ? AND m.at < ?`,
		from.Unix(), to.Unix()).Scan(&c.Opening); err != nil {
		return c, err
	}

	c.DeltaISK = c.ISKTo - c.ISKFrom
	c.DeltaStock = c.StockTo - c.StockFrom
	c.Residual = c.DeltaISK - c.Explained
	// Инвентаризация создаёт склад из ничего — это не заработок.
	c.Profit = c.Explained + c.DeltaStock - c.Opening
	return c, nil
}

// iskAt sums the last known balance of every wallet at an instant. The
// journal carries a running balance, so this is exact wherever the
// journal reaches — and simply zero before the collector started.
func (s *Store) iskAt(t time.Time) (float64, error) {
	var v float64
	err := s.db.QueryRow(`
WITH last AS (
  SELECT owner_id, division, balance,
         ROW_NUMBER() OVER (PARTITION BY owner_id, division
                            ORDER BY date DESC, id DESC) AS rn
  FROM hist_journal WHERE date <= ?
)
SELECT COALESCE(SUM(balance), 0) FROM last WHERE rn = 1`, t.Unix()).Scan(&v)
	return v, err
}

// stockAt is what the shelf cost at an instant. Balances are never
// stored, so it is the sum of every movement up to that point — which is
// also why a rebuild can never drift from the documents.
func (s *Store) stockAt(t time.Time) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost), 0) FROM acc_move
		WHERE at <= ?`, t.Unix()).Scan(&v)
	return v, err
}

// ── вклад по переделам ───────────────────────────────────────────────

// StageLine is what one kind of activity added over the period.
type StageLine struct {
	Kind     string
	Docs     int
	InMkt    float64 // рыночная оценка того, что зашло
	OutMkt   float64 // рыночная оценка того, что вышло
	Added    float64 // OutMkt − InMkt: вклад этого передела
	CostMove float64 // сколько себестоимости через него прошло
}

// Stages splits value added by activity.
//
// Без этого разреза вся прибыль цепочки «копал → перерабатывал → строил →
// продал» падает на последний шаг, и вопрос «выгодно ли было производить»
// остаётся без ответа (§9.3). Считается по рыночной оценке на момент
// движения — она едет в acc_move.mkt рядом с себестоимостью.
func (s *Store) Stages(from, to time.Time) ([]StageLine, error) {
	rows, err := s.db.Query(`
SELECT d.kind, COUNT(DISTINCT d.id),
       COALESCE(-SUM(CASE WHEN m.qty < 0 THEN m.mkt END), 0),
       COALESCE(SUM(CASE WHEN m.qty > 0 THEN m.mkt END), 0),
       COALESCE(SUM(ABS(m.cost)), 0)
FROM acc_doc d JOIN acc_move m ON m.doc_id = d.id
WHERE d.at >= ? AND d.at < ? AND d.kind NOT IN ('opening')
GROUP BY d.kind
ORDER BY 4 DESC`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageLine
	for rows.Next() {
		var l StageLine
		if err := rows.Scan(&l.Kind, &l.Docs, &l.InMkt, &l.OutMkt, &l.CostMove); err != nil {
			return nil, err
		}
		l.Added = l.OutMkt - l.InMkt
		out = append(out, l)
	}
	return out, rows.Err()
}

// UnmatchedFees counts broker fees that could not be tied to an order.
// ESI gives the fee no context at all, so the only handle is the moment
// the order was placed — and an order older than the history window
// leaves its fee homeless (§8).
func (s *Store) UnmatchedFees() (int, float64, error) {
	var n int
	var sum float64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(-SUM(c.amount),0)
		FROM acc_doc d JOIN acc_cash c ON c.doc_id = d.id
		WHERE d.kind = 'fee' AND d.note LIKE 'сбор не сопоставлен%'`).Scan(&n, &sum)
	return n, sum, err
}
