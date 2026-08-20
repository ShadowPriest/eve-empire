package pi

// RefineryDonor is a hand-built P0→P1 colony used as the geometry
// template for every generated refinery: a launchpad with five
// factories around it, each carrying one or two more behind it.
// Coordinates come from a real in-game layout, so link lengths (and
// therefore powergrid cost) stay low. The extractor of the original was
// removed — extractor placement depends on the planet's resource
// hotspots and is done by hand.
const refineryDonorJSON = `{"CmdCtrLv": 5, "Cmt": "donor", "Diam": 4460.0,
"L": [{"D": 2, "Lv": 0, "S": 1}, {"D": 3, "Lv": 0, "S": 1}, {"D": 4, "Lv": 0, "S": 1},
      {"D": 5, "Lv": 0, "S": 1}, {"D": 6, "Lv": 0, "S": 1}, {"D": 7, "Lv": 0, "S": 2},
      {"D": 8, "Lv": 0, "S": 4}, {"D": 9, "Lv": 0, "S": 3}, {"D": 10, "Lv": 0, "S": 5},
      {"D": 11, "Lv": 0, "S": 6}, {"D": 12, "Lv": 0, "S": 6}, {"D": 13, "Lv": 0, "S": 6}],
"P": [{"H": 0, "La": 1.7244, "Lo": 4.53465, "S": null, "T": 2544},
      {"H": 0, "La": 1.72057, "Lo": 4.54618, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.71949, "Lo": 4.5234, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.73245, "Lo": 4.54407, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.73186, "Lo": 4.524, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.74028, "Lo": 4.53295, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.72415, "Lo": 4.55812, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.73636, "Lo": 4.55602, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.72327, "Lo": 4.51056, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.73535, "Lo": 4.51001, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.74499, "Lo": 4.54457, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.75288, "Lo": 4.53198, "S": 2399, "T": 2473},
      {"H": 0, "La": 1.74469, "Lo": 4.52018, "S": 2399, "T": 2473}],
"Pln": 2016,
"R": [{"P": [1, 2], "Q": 3000, "T": 2270}, {"P": [2, 1], "Q": 20, "T": 2399},
      {"P": [1, 3], "Q": 3000, "T": 2270}, {"P": [3, 1], "Q": 20, "T": 2399},
      {"P": [1, 4], "Q": 3000, "T": 2270}, {"P": [4, 1], "Q": 20, "T": 2399},
      {"P": [1, 5], "Q": 3000, "T": 2270}, {"P": [5, 1], "Q": 20, "T": 2399},
      {"P": [1, 6], "Q": 3000, "T": 2270}, {"P": [6, 1], "Q": 20, "T": 2399},
      {"P": [1, 2, 7], "Q": 3000, "T": 2270}, {"P": [7, 2, 1], "Q": 20, "T": 2399},
      {"P": [1, 4, 8], "Q": 3000, "T": 2270}, {"P": [8, 4, 1], "Q": 20, "T": 2399},
      {"P": [1, 3, 9], "Q": 3000, "T": 2270}, {"P": [9, 3, 1], "Q": 20, "T": 2399},
      {"P": [1, 5, 10], "Q": 3000, "T": 2270}, {"P": [10, 5, 1], "Q": 20, "T": 2399},
      {"P": [1, 6, 11], "Q": 3000, "T": 2270}, {"P": [11, 6, 1], "Q": 20, "T": 2399},
      {"P": [1, 6, 12], "Q": 3000, "T": 2270}, {"P": [12, 6, 1], "Q": 20, "T": 2399},
      {"P": [1, 6, 13], "Q": 3000, "T": 2270}, {"P": [13, 6, 1], "Q": 20, "T": 2399}]}`

// RefineryDonor returns the built-in donor layout (12 basic factories
// around one launchpad, no extractor).
func RefineryDonor() (*Template, error) { return Parse([]byte(refineryDonorJSON)) }
