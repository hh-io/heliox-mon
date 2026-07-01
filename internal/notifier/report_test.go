package notifier

import "testing"

func TestBillingUsed(t *testing.T) {
	const tx, rx int64 = 100, 250
	cases := map[string]int64{
		"bidirectional": tx + rx,
		"tx_only":       tx,
		"rx_only":       rx,
		"max_value":     rx,
		"unknown":       tx + rx, // 未知模式回退到双向
	}
	for mode, want := range cases {
		if got := billingUsed(mode, tx, rx); got != want {
			t.Errorf("billingUsed(%q)=%d，期望 %d", mode, got, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 B",
		512:                    "512 B",
		1024:                   "1.00 KiB",
		1024 * 1024:            "1.00 MiB",
		1024 * 1024 * 1024:     "1.00 GiB",
		3 * 1024 * 1024 * 1024: "3.00 GiB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d)=%q，期望 %q", in, got, want)
		}
	}
}
