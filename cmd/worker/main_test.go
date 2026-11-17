package main

import (
	"testing"
	"time"
)

func TestStepPause(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		warn bool
	}{
		{"", 2 * time.Second, false},
		{"2s", 2 * time.Second, false},
		{"150ms", 150 * time.Millisecond, false},
		{"0", 0, false},
		{"abc", 2 * time.Second, true},
		{"-1s", 2 * time.Second, true},
		{"2", 2 * time.Second, true},
	}
	for _, c := range cases {
		got, err := stepPause(c.raw)
		if got != c.want {
			t.Errorf("STEP_PAUSE %q: %s, want %s", c.raw, got, c.want)
		}
		if (err != nil) != c.warn {
			t.Errorf("STEP_PAUSE %q: warning %v, want one: %v", c.raw, err, c.warn)
		}
	}
}
