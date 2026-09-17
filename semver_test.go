package selfupdate

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"v1.0.1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.1", false},
		{"v1.2.0", "v1.10.0", false},
		{"v1.10.0", "v1.2.0", true},
		{"v2.0.0", "v1.99.99", true},
		{"1.0.1", "v1.0.0", true},
		{"v1.0.0", "dev", true},
		{"v0.0.1", "dev", true},
		{"v1.0.0", "", true},
		{"", "v1.0.0", false},
		{"v1.0.0", "v1.0.0-rc1", true},
		{"v1.0.0-rc1", "v1.0.0", false},
		{"v1.0.0-rc2", "v1.0.0-rc1", true},
		{"v1.1", "v1.0.9", true},
		{"v1.0.0+build9", "v1.0.0+build1", true},
	}

	for _, c := range cases {
		if got := Newer(c.candidate, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestCompareIsSymmetric(t *testing.T) {
	pairs := [][2]string{{"v1.0.0", "v2.0.0"}, {"v1.2.3", "v1.2.4"}, {"v1.0.0-rc1", "v1.0.0"}}

	for _, p := range pairs {
		if Compare(p[0], p[1]) != -Compare(p[1], p[0]) {
			t.Errorf("Compare(%q,%q) and its reverse disagree", p[0], p[1])
		}
	}

	if Compare("v1.0.0", "1.0.0") != 0 {
		t.Error("the leading v changed the ordering")
	}
}
