package gonkcfg

import (
	"testing"

	"gopkg.in/yaml.v3"
)

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

func TestUnmarshalYAML(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		want    TokenQuantity
		wantErr bool
	}{
		{"bare int", "q: 50000", 50_000, false},
		{"quoted suffixed string", `q: "50M"`, 50_000_000, false},
		{"unquoted suffixed scalar", "q: 50M", 50_000_000, false},
		{"zero", "q: 0", 0, false},
		{"null is unset", "q: null", 0, false},
		{"negative int", "q: -5", 0, true},
		{"float", "q: 1.5", 0, true},
		{"exponent float", "q: 1e3", 0, true},
		{"integral float", "q: 2.0", 0, true},
		{"bool", "q: true", 0, true},
		{"sequence", "q: [1, 2]", 0, true},
		{"bad string", `q: "50m"`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got struct {
				Q TokenQuantity `yaml:"q"`
			}
			err := yaml.Unmarshal([]byte(c.yaml), &got)
			if c.wantErr != (err != nil) {
				t.Fatalf("Unmarshal(%q) err = %v, wantErr %v", c.yaml, err, c.wantErr)
			}
			if !c.wantErr && got.Q != c.want {
				t.Fatalf("Unmarshal(%q) = %d, want %d", c.yaml, got.Q, c.want)
			}
		})
	}
}
