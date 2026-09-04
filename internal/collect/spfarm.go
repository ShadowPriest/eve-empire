package collect

import (
	"context"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/store"
)

// Цены SP-фермы. Публичные маршруты, персонажи не нужны: PLEX живёт на
// глобальном рынке, экстрактор и инжектор — в Жите. Снапшоты копятся в
// spfarm_price и переживают 13-месячное окно истории ESI (см. store/spfarm.go).
var farmGoods = []struct{ typeID, region int64 }{
	{esi.TypePLEX, esi.RegionPLEX},
	{esi.TypeSkillExtractor, esi.RegionTheForge},
	{esi.TypeLargeInjector, esi.RegionTheForge},
}

func (c *Collector) SPFarmPrices(ctx context.Context) error {
	started := time.Now()
	var errs []string
	var snaps []store.FarmSnap
	for _, g := range farmGoods {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err := c.ESI.RegionOrderStats(g.region, g.typeID)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if st.SellMin == 0 && st.BuyMax == 0 {
			continue // пустой стакан — нечего писать
		}
		snaps = append(snaps, store.FarmSnap{
			TypeID: g.typeID, At: started,
			SellMin: st.SellMin, SellP98: st.SellP98,
			BuyMax: st.BuyMax, BuyP98: st.BuyP98,
		})
	}
	if err := c.Store.SaveFarmSnaps(snaps); err != nil {
		errs = append(errs, err.Error())
	}
	return c.note("spfarm", started, len(snaps), errs)
}
