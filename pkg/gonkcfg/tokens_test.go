package gonkcfg

import "testing"

func TestParseTokenQuantity(t *testing.T) {
	cases := []struct {
		in      string
		want    TokenQuantity
		wantErr bool
	}{
		{"0", 0, false},
		{"12345", 12345, false},
		{"50K", 50_000, false},
		{"50M", 50_000_000, false},
		{"2G", 2_000_000_000, false},
		{"", 0, true},
		{"-5", 0, true},
		{"50m", 0, true},   // lowercase suffix rejected
		{"1.5M", 0, true},  // fractional rejected
		{"M", 0, true},
		{"50MB", 0, true},
		{"9223372036854775807K", 0, true}, // overflow
	}
	for _, c := range cases {
		got, err := ParseTokenQuantity(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("ParseTokenQuantity(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseTokenQuantity(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
