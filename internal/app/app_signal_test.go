package app

import (
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestSignalNameMapping(t *testing.T) {
	cases := []struct {
		sig  os.Signal
		want string
	}{
		{os.Interrupt, "SIGINT"},
		{syscall.SIGTERM, "SIGTERM"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct {
			sig  os.Signal
			want string
		}{syscall.Signal(21), "SIGBREAK"})
	} else {
		cases = append(cases, struct {
			sig  os.Signal
			want string
		}{syscall.Signal(1), "SIGHUP"})
	}
	for _, tc := range cases {
		if got := signalName(tc.sig); got != tc.want {
			t.Fatalf("signalName(%v) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}
