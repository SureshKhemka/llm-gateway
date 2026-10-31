package cache

import "testing"

// Covers: CCH-09
func TestCCH09_SalientGuardSeparatesLookalikePrompts(t *testing.T) {
	same := [][2]string{
		{"What are the opening hours of Lotus Bakery?", "Quick question — what are the opening hours of Lotus Bakery?"},
		{"What are the opening hours of Lotus Bakery?", "Could you please help with this: What are the opening hours of Lotus Bakery? Thanks!"},
		{"Book a table for 4 people at Saffron House at 7:30 pm tonight.", "Hey, book a table for 4 people at Saffron House at 7:30 pm tonight."},
	}
	for _, p := range same {
		if Salient(p[0]) != Salient(p[1]) {
			t.Errorf("paraphrases differ:\n %q → %s\n %q → %s", p[0], Salient(p[0]), p[1], Salient(p[1]))
		}
	}
	different := [][2]string{
		{"Extract the invoice number: Invoice INV-2041 total ₹12,400", "Extract the invoice number: Invoice INV-7730 total ₹12,400"},
		{"How much change from a ₹500 note for 3 pens at ₹40?", "How much change from a ₹500 note for 4 pens at ₹40?"},
		{"Where is Banyan Clinic located?", "Where is Lotus Bakery located?"},
		{`Classify the review "great value"`, `Classify the review "broke in a day"`},
	}
	for _, p := range different {
		if Salient(p[0]) == Salient(p[1]) {
			t.Errorf("look-alikes share a fingerprint: %q / %q → %s", p[0], p[1], Salient(p[0]))
		}
	}
}
