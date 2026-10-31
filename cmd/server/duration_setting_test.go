package main

import (
	"testing"
	"time"
)

func TestPositiveDuration(t *testing.T) {
	def := 10 * time.Second
	for _, tt := range []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"1m", time.Minute},
		{"", def},
		{"nonsense", def},
		{"0", def},
		{"0s", def},
		{"-5s", def},
	} {
		if got := positiveDuration(tt.in, def); got != tt.want {
			t.Errorf("positiveDuration(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
