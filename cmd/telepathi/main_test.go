package main

import "testing"

func TestSafeFileName(t *testing.T) {
	ok := map[string]string{
		"file.dat":         "file.dat",
		"../../etc/passwd": "passwd",
		"/abs/path/x.bin":  "x.bin",
		"dir/../y":         "y",
	}
	for in, want := range ok {
		got, err := safeFileName(in)
		if err != nil || got != want {
			t.Errorf("safeFileName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "..", "/", "../.."} {
		if got, err := safeFileName(in); err == nil {
			t.Errorf("safeFileName(%q) = %q; want error", in, got)
		}
	}
}
