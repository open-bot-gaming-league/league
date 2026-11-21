package engine

import "testing"

// Category counts over every 5-card and every 7-card hand are standard
// published numbers; matching them all is strong evidence the evaluator is
// right.
func TestHoldemStrengthFiveCardCategoryCounts(t *testing.T) {
	want := [9]int{1302540, 1098240, 123552, 54912, 10200, 5108, 3744, 624, 40}
	var got [9]int
	forEachCombo(52, 5, 0, func(cs []Card) { got[holdemStrength(cs)>>catShift]++ })
	if got != want {
		t.Fatalf("5-card category counts\n got %v\nwant %v", got, want)
	}
}

func TestHoldemStrengthSevenCardCategoryCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("133,784,560 hands")
	}
	want := [9]int{23294460, 58627800, 31433400, 6461620, 6180020, 4047644, 3473184, 224848, 41584}
	var got [9]int
	forEachCombo(52, 7, 0, func(cs []Card) { got[holdemStrength(cs)>>catShift]++ })
	if got != want {
		t.Fatalf("7-card category counts\n got %v\nwant %v", got, want)
	}
}

func card(s string) Card {
	const ranks, suits = "23456789TJQKA", "cdhs"
	var r, u int
	for i := range ranks {
		if ranks[i] == s[0] {
			r = i
		}
	}
	for i := range suits {
		if suits[i] == s[1] {
			u = i
		}
	}
	return Card(r*4 + u)
}

func hand(ss ...string) []Card {
	var cs []Card
	for _, s := range ss {
		cs = append(cs, card(s))
	}
	return cs
}

// Orderings inside a category, including the cases that are easy to get
// wrong: kickers, the wheel, the best five of seven.
func TestHoldemStrengthOrdering(t *testing.T) {
	pairs := [][2][]Card{
		{hand("7c", "5d", "4h", "3s", "2c"), hand("Ac", "Kd", "Qh", "Js", "9c")},
		{hand("Ac", "Kd", "Qh", "Js", "9c"), hand("2c", "2d", "5h", "4s", "3c")},
		{hand("Ac", "Ad", "Kh", "Qs", "Tc"), hand("Ac", "Ad", "Kh", "Qs", "Jc", "3d", "2h")},
		{hand("Ac", "Ad", "Kh", "Ks", "Jc"), hand("Ac", "Ad", "Kh", "Ks", "Qc", "Qd", "2h")},
		{hand("Ac", "Ad", "Kh", "Ks", "Qc"), hand("2c", "2d", "2h", "4s", "3c")},
		{hand("Ac", "Ad", "Ah", "Ks", "Qc"), hand("Ac", "2d", "3h", "4s", "5c")}, // trips < wheel
		{hand("Ac", "2d", "3h", "4s", "5c"), hand("2c", "3d", "4h", "5s", "6c")}, // wheel < six-high straight
		{hand("Tc", "Jd", "Qh", "Ks", "Ac"), hand("2c", "3c", "4c", "5c", "7c")}, // straight < flush
		{hand("2c", "3c", "4c", "5c", "7c"), hand("2c", "3c", "4c", "5c", "8c")},
		{hand("Ac", "Kc", "Qc", "Jc", "9c"), hand("2c", "2d", "2h", "3s", "3c")},                             // flush < full house
		{hand("Ac", "Ad", "Ah", "Ks", "Kc", "Kd", "2h")[3:], hand("Ac", "Ad", "Ah", "Ks", "Kc", "Kd", "2h")}, // KKK2 trips < AAAKK
		{hand("Ac", "Ad", "Ah", "Ks", "Kc"), hand("2c", "2d", "2h", "2s", "3c")},
		{hand("2c", "2d", "2h", "2s", "3c"), hand("2c", "2d", "2h", "2s", "Ac", "Kd", "Qh")}, // quad kicker
		{hand("Ac", "Ad", "Ah", "As", "Kc"), hand("Ac", "2c", "3c", "4c", "5c")},             // quads < steel wheel
		{hand("Ac", "2c", "3c", "4c", "5c"), hand("9c", "Tc", "Jc", "Qc", "Kc", "Ad", "Ah")},
	}
	for i, p := range pairs {
		if lo, hi := holdemStrength(p[0]), holdemStrength(p[1]); lo >= hi {
			t.Errorf("pair %d: want %v < %v, got %#x >= %#x", i, p[0], p[1], lo, hi)
		}
	}
	// Suits never matter between equal ranks.
	if a, b := holdemStrength(hand("Ac", "Kd", "Qh", "Js", "9c")), holdemStrength(hand("Ad", "Kh", "Qs", "Jc", "9d")); a != b {
		t.Errorf("same ranks, different suits: %#x != %#x", a, b)
	}
}
