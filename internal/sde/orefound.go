package sde

// Where each rock spawns. This is NOT in the SDE in any form — CCP ships
// the composition of every ore but never says where it can be mined, so
// the table is hand-kept, like rawByPlanet above. Keyed by the English
// base name of the family; grades and compressed twins inherit it.
//
// Labels follow the game's own vocabulary: High/Low/Null are security
// bands of the four empires' space, Anom means the ore only shows up in
// mining anomalies (not in belts), Sov marks anomalies that need an
// upgraded sovereignty hub, A0 the special systems around A0 stars.
var foundIn = map[string][]FoundTag{
	// ── high-sec ──
	"Veldspar":    {{"High", "Хайсек любой империи"}, {"Anom", "Аномалии хайсека"}},
	"Scordite":    {{"High", "Хайсек любой империи"}, {"Anom", "Аномалии хайсека"}},
	"Plagioclase": {{"High", "Хайсек Калдари, Галленте, Минматар"}},
	"Pyroxeres":   {{"All", "Всё пространство Амарр и Калдари"}, {"Null", "Нули Минматар"}, {"Anom", "Аномалии нулей"}, {"WH", "Червоточины"}},
	"Omber":       {{"Low", "Лоусек Галленте и Минматар"}, {"Anom", "Аномалии нулей"}, {"WH", "Червоточины"}},
	"Kernite":     {{"Low+Null", "Лоусек и нули Амарр, Калдари, Минматар"}, {"Anom", "Аномалии нулей"}, {"WH", "Червоточины"}},

	// ── low-sec ──
	"Jaspet":     {{"Low", "Лоусек Амарр и Галленте"}, {"Anom", "Аномалии лоусека"}},
	"Hemorphite": {{"Low", "Лоусек Амарр и Галленте"}, {"Anom", "Аномалии лоусека"}},
	"Hedbergite": {{"Low", "Лоусек Калдари и Минматар"}, {"Anom", "Аномалии лоусека"}},

	// ── null-sec ──
	"Gneiss":       {{"Anom", "Аномалии лоусека и нулей"}, {"WH", "Червоточины"}},
	"Dark Ochre":   {{"Anom", "Аномалии лоусека и нулей"}, {"0.5", "Системы 0.5 на границе с лоусеком"}},
	"Crokite":      {{"Anom", "Аномалии лоусека и нулей"}, {"0.5", "Системы 0.5 на границе с лоусеком"}},
	"Bistot":       {{"Null", "Нули любой империи"}, {"Anom", "Аномалии нулей"}, {"WH", "Червоточины"}},
	"Arkonor":      {{"Null", "Нули Амарр, Галленте, Минматар"}, {"Anom", "Аномалии нулей"}, {"WH", "Червоточины"}},
	"Spodumain":    {{"Null", "Нули любой империи"}, {"Anom", "Аномалии нулей"}, {"WH", "Червоточины"}},
	"Mercoxit":     {{"Null", "Нули любой империи"}, {"Anom", "Аномалии нулей"}},
	"Prismaticite": {{"Low+Null", "Лоусек и нули любой империи"}},

	// ── sovereignty-hub anomalies (Rogue Drone / EDENCOM era ores) ──
	"Griemeer": {{"Anom", "Аномалии нулей с прокачанным хабом суверенитета"}},
	"Hezorime": {{"Anom", "Аномалии нулей с прокачанным хабом суверенитета"}},
	"Kylixium": {{"Anom", "Аномалии нулей с прокачанным хабом суверенитета"}},
	"Nocxite":  {{"Anom", "Аномалии нулей с прокачанным хабом суверенитета"}},
	"Ueganite": {{"Anom", "Аномалии нулей с прокачанным хабом суверенитета"}},

	// ── A0-star systems and 0.5 border systems ──
	"Mordunium": {{"A0 + 0.5", "Нули и червоточины с звездой A0, плюс системы 0.5 на границе с лоусеком"}},
	"Ytirium":   {{"A0 + 0.5", "Нули и червоточины с звездой A0, плюс системы 0.5 на границе с лоусеком"}},
	"Eifyrium":  {{"A0 + 0.5", "Нули и червоточины с звездой A0, плюс системы 0.5 на границе с лоусеком"}},
	"Ducinium":  {{"A0 + 0.5", "Нули и червоточины с звездой A0, плюс системы 0.5 на границе с лоусеком"}},

	// ── Triglavian space ──
	"Bezdnacine":  {{"Триглав", "Почвен"}},
	"Rakovene":    {{"Триглав", "Почвен"}},
	"Talassonite": {{"Триглав", "Почвен"}},

	// ── ice ──
	"Blue Ice":     {{"All", "Всё пространство Галленте"}},
	"Clear Icicle": {{"All", "Всё пространство Амарр"}},
	"Glacial Mass": {{"All", "Всё пространство Минматар"}},
	"White Glaze":  {{"All", "Всё пространство Калдари"}},
	"Glare Crust":  {{"Low", "Лоусек любой империи"}, {"WH", "Червоточины"}},
	"Dark Glitter": {{"Low", "Лоусек любой империи"}, {"WH", "Червоточины"}},
	"Gelidus":      {{"Null", "Нули любой империи"}, {"WH", "Червоточины"}},
	"Krystallos":   {{"Null", "Нули любой империи"}, {"WH", "Червоточины"}},

	// ── gas ──
	"Amber Cytoserocin":      {{"Low+Null", "Лоусек и нули Калдари"}},
	"Golden Cytoserocin":     {{"Low+Null", "Лоусек и нули Калдари"}},
	"Azure Cytoserocin":      {{"Low+Null", "Лоусек и нули Минматар"}},
	"Vermillion Cytoserocin": {{"Low+Null", "Лоусек и нули Минматар"}},
	"Celadon Cytoserocin":    {{"Low+Null", "Лоусек и нули Галленте"}},
	"Viridian Cytoserocin":   {{"Low+Null", "Лоусек и нули Галленте"}},
	"Lime Cytoserocin":       {{"Low+Null", "Лоусек и нули Амарр"}},
	"Malachite Cytoserocin":  {{"Low+Null", "Лоусек и нули Амарр"}},
	"Amber Mykoserocin":      {{"Low", "Лоусек Калдари и Галленте"}, {"Null", "Нули дронов"}},
	"Golden Mykoserocin":     {{"Low", "Лоусек Калдари и Галленте"}, {"Null", "Нули дронов"}},
	"Azure Mykoserocin":      {{"Low", "Лоусек Минматар и Амарр"}, {"Null", "Нули Минматар"}},
	"Vermillion Mykoserocin": {{"Low", "Лоусек Амарр и Минматар"}, {"Null", "Нули Минматар"}},
	"Celadon Mykoserocin":    {{"Low", "Лоусек Галленте и Амарр"}, {"Null", "Нули Галленте и Калдари"}},
	"Viridian Mykoserocin":   {{"Low", "Лоусек Галленте и Амарр"}, {"Null", "Нули Калдари и Галленте"}},
	"Lime Mykoserocin":       {{"Low", "Лоусек Амарр и Галленте"}, {"Null", "Нули Минматар"}},
	"Malachite Mykoserocin":  {{"Low", "Лоусек Амарр и Галленте"}, {"Null", "Нули Минматар"}},
}

// wormholeGas tags every fullerite, which only ever spawns in J-space.
func init() {
	for _, n := range []string{
		"Fullerite-C28", "Fullerite-C32", "Fullerite-C320", "Fullerite-C50",
		"Fullerite-C540", "Fullerite-C60", "Fullerite-C70", "Fullerite-C72",
		"Fullerite-C84",
	} {
		foundIn[n] = []FoundTag{{"WH", "Червоточины"}}
	}
}
