package debversion

import "testing"

// TestCompare is a truth table mirroring `dpkg --compare-versions a OP b`.
// Each case asserts the sign of Compare(a, b). Cases are grouped by the
// dpkg pitfall they exercise; the Docker/Logstash rows are the real
// version strings from the incident that motivated this package.
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int // -1, 0, +1 (sign of Compare(a,b))
	}{
		// equality
		{"1.0", "1.0", 0},
		{"1.2.3", "1.2.3", 0},

		// plain numeric ordering
		{"1.0", "1.1", -1},
		{"1.1", "1.0", 1},
		{"2.0", "10.0", -1}, // numeric, not lexical

		// leading-zero numeric runs compare numerically
		{"1.00", "1.0", 0},
		{"1.01", "1.1", 0},
		{"1.010", "1.10", 0},

		// epoch dominates
		{"1:0", "2.0", 1}, // epoch 1 > epoch 0
		{"2:1.0", "10:1.0", -1},
		{"1:1.0", "1.0", 1},
		{"0:1.0", "1.0", 0}, // explicit epoch 0 == no epoch

		// debian revision split on the LAST hyphen
		{"1.0-1", "1.0-2", -1},
		{"1.0-1", "1.0-1", 0},
		{"1.0", "1.0-1", -1},       // missing revision < -1
		{"1.0-0", "1.0", 0},        // -0 == missing revision
		{"1.2-3-4", "1.2-3-5", -1}, // upstream "1.2-3", revision "4" vs "5"

		// '~' sorts before everything, including end-of-string
		{"1.0~rc1", "1.0", -1},
		{"1.0~rc1", "1.0~rc2", -1},
		{"1.0~~", "1.0~", -1},
		{"1.0~", "1.0", -1},
		{"1.0~beta", "1.0~beta1", -1},

		// letters sort before non-letter punctuation; '+' is punctuation
		{"1.0a", "1.0+", -1}, // 'a' (letter) < '+' (punct)
		{"1.0+dfsg", "1.0", 1},
		{"1.0+dfsg1", "1.0+dfsg2", -1},

		// letter vs digit boundary
		{"1.0", "1.0a", -1}, // end-of-segment < letter? end(0) vs 'a': 0 < 'a'

		// Ubuntu revisions
		{"5.4.0-1ubuntu1", "5.4.0-1ubuntu2", -1},
		{"1.2-1ubuntu0.1", "1.2-1ubuntu0.2", -1},
		{"1.2-1", "1.2-1ubuntu0.1", -1},

		// Docker (real strings from the incident)
		{"5:24.0.7-1~ubuntu.22.04~jammy", "5:24.0.7-1~ubuntu.22.04~jammy", 0},
		{"5:24.0.6-1~ubuntu.22.04~jammy", "5:24.0.7-1~ubuntu.22.04~jammy", -1},
		{"5:19.03.0~3-0~ubuntu", "5:24.0.7-1~ubuntu.22.04~jammy", -1},
		{"5:20.10.24~3-0~ubuntu-jammy", "5:24.0.7-1~ubuntu.22.04~jammy", -1},

		// Logstash (real strings from the incident)
		{"1:8.13.4-1", "1:8.13.4-1", 0},
		{"1:8.0.0-1", "1:8.13.4-1", -1},
		{"1:8.13.3-1", "1:8.13.4-1", -1},
		{"1:8.13.10-1", "1:8.13.4-1", 1}, // 13.10 > 13.4 numerically
	}

	for _, c := range cases {
		got := sign(Compare(c.a, c.b))
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		// Antisymmetry: Compare(b, a) must be the negation.
		rev := sign(Compare(c.b, c.a))
		if rev != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d (antisymmetry)", c.b, c.a, rev, -c.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
