package main

import (
	"strings"
	"testing"
)

func TestPluginMemoryFlag(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  uint64
	}{
		{"0", 0}, {"65536", 65536}, {"65536B", 65536},
		{"64KiB", 64 << 10}, {"256MiB", 256 << 20}, {"4GiB", 4 << 30},
		{"18446744073709551615", ^uint64(0)},
	} {
		var got uint64
		flag := pluginMemoryFlag{&got}
		if err := flag.Set(tc.input); err != nil || got != tc.want {
			t.Fatalf("%s: got %d, want %d: %v", tc.input, got, tc.want, err)
		}
	}
	for _, input := range []string{"", "-1", "+1", "1.5MiB", "256MB", "256 MiB", "MiB", "18446744073709551616", "18446744073709551615KiB"} {
		got := uint64(65536)
		if err := (pluginMemoryFlag{&got}).Set(input); err == nil || got != 65536 {
			t.Fatalf("invalid size %q changed value to %d: %v", input, got, err)
		}
	}
	var got uint64
	if err := (pluginMemoryFlag{&got}).Set("18446744073709551615GiB"); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("missing overflow diagnostic: %v", err)
	}
}
