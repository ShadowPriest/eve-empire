package main

import (
	"fmt"
	"log"
	"time"

	"eve-empire/internal/config"
	"eve-empire/internal/esi"
	"eve-empire/internal/ledger"
	"eve-empire/internal/sso"
	"eve-empire/internal/store"
)

// runLedger builds the ledger from collected history and prints the
// stage-1 reports. A dev helper: the same calls sit behind the page.
func runLedger(priceSource string, report bool) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	ec := esi.New(sso.New(cfg.ClientID, cfg.ClientSecret, cfg.CallbackURL, cfg.Scopes, cfg.UserAgent), st, cfg.UserAgent)
	ec.SetLanguage(st.Setting("language"))

	if priceSource != "" {
		started := time.Now()
		res, err := ledger.New(st, ec).BuildAll(priceSource)
		if err != nil {
			log.Fatalf("сборка реестра: %v", err)
		}
		fmt.Println("СБОРКА за", time.Since(started).Round(time.Millisecond))
		fmt.Println("  документов:", res.Documents, " партий:", res.Lots,
			" пропущено (уже проведено):", res.Skipped)
		fmt.Println(" ", res.Note)
		fmt.Println()
	}
	if !report {
		return
	}

	a, err := st.Attention()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("РЕЕСТР: документов %d, живых партий %d, склад по себестоимости %s\n",
		a.Documents, a.Lots, isk(a.StockCost))
	fmt.Printf("  из них оценочных партий %d на %s\n\n", a.EstimateLots, isk(a.EstimateCost))

	from := time.Now().AddDate(0, 0, -30)
	to := time.Now().Add(time.Hour)

	margins, err := st.Margins(from, to)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("МАРЖА ЗА 30 ДНЕЙ (реализованная, без переоценки остатка)")
	if len(margins) == 0 {
		fmt.Println("  продаж в реестре нет")
	}
	var ids []int64
	for _, m := range margins {
		ids = append(ids, m.TypeID)
	}
	names := ec.Names(ids)
	var revenue, cogs, tax, profit float64
	fmt.Printf("  %-34s %6s %14s %14s %12s %8s\n", "тип", "сделок", "выручка", "себестоимость", "прибыль", "маржа")
	for i, m := range margins {
		revenue += m.Revenue
		cogs += m.COGS
		tax += m.Tax
		profit += m.Profit()
		if i < 12 {
			fmt.Printf("  %-34s %6d %14s %14s %12s %7.1f%%\n",
				trim34(names[m.TypeID]), m.Sales, isk(m.Revenue), isk(m.COGS),
				isk(m.Profit()), m.MarginPct())
		}
	}
	if len(margins) > 12 {
		fmt.Printf("  … ещё %d типов\n", len(margins)-12)
	}
	fmt.Printf("  ИТОГО выручка %s, себестоимость %s, налог %s, прибыль %s\n\n",
		isk(revenue), isk(cogs), isk(tax), isk(profit))

	stock, err := st.Stock()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("СКЛАД ПО СЕБЕСТОИМОСТИ (топ-10 позиций)")
	ids = ids[:0]
	for _, s := range stock {
		ids = append(ids, s.TypeID)
	}
	names = ec.Names(ids)
	for i, s := range stock {
		if i >= 10 {
			break
		}
		mark := ""
		if s.Estimated {
			mark = " (оценка)"
		}
		where := s.HolderName
		if where == "" {
			where = "ангар"
		}
		fmt.Printf("  %-34s %10d  %14s  %s%s\n",
			trim34(names[s.TypeID]), s.Quantity, isk(s.Cost), where, mark)
	}
}

func trim34(s string) string {
	if s == "" {
		return "—"
	}
	r := []rune(s)
	if len(r) > 34 {
		return string(r[:33]) + "…"
	}
	return s
}

// isk formats ISK the way the game does: thousands apart, two decimals
// only when the number is small enough for them to mean anything.
func isk(v float64) string {
	switch a := abs(v); {
	case a >= 1e9:
		return fmt.Sprintf("%.2f млрд", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.2f млн", v/1e6)
	case a >= 1e3:
		return fmt.Sprintf("%.1f тыс", v/1e3)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
