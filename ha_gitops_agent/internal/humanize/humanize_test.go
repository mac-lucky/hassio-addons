package humanize

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0:              "0 B",
		512:            "512 B",
		1023:           "1023 B",
		1024:           "1.0 KB",
		1536:           "1.5 KB",
		100 << 20:      "100.0 MB",
		10 << 20:       "10.0 MB",
		10*(1<<20) + 1: "10.0 MB",
		3 * (1 << 30):  "3.0 GB",
	}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                     "0.0s",
		-3 * time.Second:                      "0.0s",
		400 * time.Millisecond:                "0.4s",
		900 * time.Millisecond:                "0.9s",
		4200 * time.Millisecond:               "4.2s",
		42 * time.Second:                      "42.0s",
		59*time.Second + 900*time.Millisecond: "59.9s",
		time.Minute:                           "1m 00s",
		72 * time.Second:                      "1m 12s",
		3*time.Minute + 12*time.Second:        "3m 12s",
		20 * time.Minute:                      "20m 00s",
	}
	for in, want := range cases {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}
