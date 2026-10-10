package ha

import (
	"strings"
	"testing"
)

func TestSafeCheckReasonDropsValues(t *testing.T) {
	in := "Invalid config for 'xiaomi_miio' at configuration.yaml, line 41: expected str for dictionary value 'token', got 'SENTINEL-value'\n" +
		"Invalid config for 'rest' at packages/net.yaml, line 3: invalid url 'https://user:SENTINEL-pw@host'\n" +
		"Invalid config for 'xiaomi_miio' at configuration.yaml, line 41: again"
	got := SafeCheckReason(in)
	if strings.Contains(got, "SENTINEL") {
		t.Fatalf("value leaked: %s", got)
	}
	want := "Invalid config for 'xiaomi_miio' at configuration.yaml, line 41; Invalid config for 'rest' at packages/net.yaml, line 3 (details in Home Assistant's log)"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := SafeCheckReason("Platform error: SENTINEL-x"); strings.Contains(got, "SENTINEL") {
		t.Fatalf("unknown shape leaked: %s", got)
	}
}
