// The price-history chart of the orders tool, drawn the way the game
// draws it: orange dots are the daily average, thin whiskers the daily
// min/max, two moving averages (5 and 20 days) run through them, the
// grey band is the 20-day Donchian channel (rolling min/max), and the
// teal bars below are the traded volume. Server-rendered SVG — the
// numbers are all known here, and the page carries no charting JS.
package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"eve-empire/internal/esi"
)

const (
	histW      = 1000.0
	histH      = 430.0
	histPad    = 14.0
	priceTop   = 14.0
	priceBot   = 292.0
	volTop     = 312.0
	volBot     = 424.0
	donchianN  = 20
	maShortN   = 5
	maLongN    = 20
	histLabelX = histW - histPad + 4
)

var ruMonths = [...]string{"янв", "фев", "мар", "апр", "май", "июн",
	"июл", "авг", "сен", "окт", "ноя", "дек"}

func renderHistoryChart(hist []esi.HistoryDay) template.HTML {
	n := len(hist)
	if n == 0 {
		return ""
	}

	// Price scale over the window, with a little headroom.
	pmin, pmax := hist[0].Lowest, hist[0].Highest
	var vmax int64
	for _, d := range hist {
		if d.Lowest < pmin {
			pmin = d.Lowest
		}
		if d.Highest > pmax {
			pmax = d.Highest
		}
		if d.Volume > vmax {
			vmax = d.Volume
		}
	}
	if pmax <= pmin {
		pmax = pmin + 1
	}
	span := pmax - pmin
	pmin -= span * 0.04
	pmax += span * 0.04

	x := func(i int) float64 {
		if n == 1 {
			return histW / 2
		}
		return histPad + float64(i)*(histW-2*histPad)/float64(n-1)
	}
	py := func(v float64) float64 {
		return priceBot - (v-pmin)/(pmax-pmin)*(priceBot-priceTop)
	}
	vy := func(v int64) float64 {
		if vmax == 0 {
			return volBot
		}
		return volBot - float64(v)/float64(vmax)*(volBot-volTop)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="histsvg" viewBox="0 0 %g %g" xmlns="http://www.w3.org/2000/svg">`, histW, histH)

	// Horizontal gridlines with price labels.
	for i := 0; i <= 4; i++ {
		v := pmin + (pmax-pmin)*float64(i)/4
		y := py(v)
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%g" y2="%.1f" stroke="#1c2536" stroke-dasharray="4 4"/>`,
			histPad, y, histW-histPad, y)
		fmt.Fprintf(&b, `<text x="%g" y="%.1f" font-size="11" fill="#5f708c" text-anchor="end">%s</text>`,
			histW-histPad-4, y-3, iskShort(v))
	}

	// Month boundaries along the bottom of the price pane.
	prevMonth := -1
	for i, d := range hist {
		day, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			continue
		}
		if m := int(day.Month()); m != prevMonth {
			if prevMonth != -1 {
				fmt.Fprintf(&b, `<line x1="%.1f" y1="%g" x2="%.1f" y2="%g" stroke="#1c2536"/>`,
					x(i), priceTop, x(i), volBot)
				fmt.Fprintf(&b, `<text x="%.1f" y="%g" font-size="11" fill="#5f708c" text-anchor="middle">%s</text>`,
					x(i), priceBot+14, ruMonths[m-1])
			}
			prevMonth = m
		}
	}

	// Donchian channel: rolling 20-day low/high band.
	if n >= 2 {
		var top, bot []string
		for i := range hist {
			lo, hi := hist[i].Lowest, hist[i].Highest
			for j := i - donchianN + 1; j < i; j++ {
				if j < 0 {
					continue
				}
				if hist[j].Lowest < lo {
					lo = hist[j].Lowest
				}
				if hist[j].Highest > hi {
					hi = hist[j].Highest
				}
			}
			top = append(top, fmt.Sprintf("%.1f,%.1f", x(i), py(hi)))
			bot = append(bot, fmt.Sprintf("%.1f,%.1f", x(i), py(lo)))
		}
		for i, j := 0, len(bot)-1; i < j; i, j = i+1, j-1 {
			bot[i], bot[j] = bot[j], bot[i]
		}
		fmt.Fprintf(&b, `<polygon points="%s %s" fill="rgba(255,255,255,.05)"/>`,
			strings.Join(top, " "), strings.Join(bot, " "))
	}

	// Daily min/max whiskers.
	for i, d := range hist {
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#55627a" stroke-width="1"/>`,
			x(i), py(d.Highest), x(i), py(d.Lowest))
	}

	// Moving averages of the daily average price.
	ma := func(window int) string {
		var pts []string
		var sum float64
		for i, d := range hist {
			sum += d.Average
			if i >= window {
				sum -= hist[i-window].Average
			}
			cnt := window
			if i+1 < window {
				cnt = i + 1
			}
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(i), py(sum/float64(cnt))))
		}
		return strings.Join(pts, " ")
	}
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#4dd2ff" stroke-width="1.4"/>`, ma(maShortN))
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#ffb547" stroke-width="1.4"/>`, ma(maLongN))

	// Average-price dots on top.
	for i, d := range hist {
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.6" fill="#ffb547"/>`, x(i), py(d.Average))
	}

	// Volume pane.
	if vmax > 0 {
		half := vy(vmax / 2)
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%g" y2="%.1f" stroke="#1c2536" stroke-dasharray="4 4"/>`,
			histPad, half, histW-histPad, half)
		fmt.Fprintf(&b, `<text x="%g" y="%.1f" font-size="11" fill="#5f708c" text-anchor="end">%s шт</text>`,
			histW-histPad-4, half-3, formatNum(vmax/2))
		bw := (histW - 2*histPad) / float64(n) * 0.62
		if bw < 1 {
			bw = 1
		}
		for i, d := range hist {
			if d.Volume == 0 {
				continue
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#2b6f92"/>`,
				x(i)-bw/2, vy(d.Volume), bw, volBot-vy(d.Volume))
		}
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
