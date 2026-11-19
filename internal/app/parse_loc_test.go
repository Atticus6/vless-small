package app

import (
	"math"
	"testing"
)

func TestParseLocRejectsNonFinite(t *testing.T) {
	for name, raw := range map[string]string{
		"nan latitude":  "NaN,114.2",
		"nan longitude": "22.3,NaN",
		"inf latitude":  "Inf,114.2",
		"negative inf":  "-Inf,114.2",
	} {
		t.Run(name, func(t *testing.T) {
			if lat, lon, ok := parseLoc(raw); ok {
				t.Fatalf("parseLoc(%q) = %v, %v, true; want rejected", raw, lat, lon)
			}
		})
	}

	lat, lon, ok := parseLoc("22.3,114.2")
	if !ok || math.Abs(lat-22.3) > 1e-9 || math.Abs(lon-114.2) > 1e-9 {
		t.Fatalf("parseLoc(valid) = %v, %v, %v", lat, lon, ok)
	}
}
