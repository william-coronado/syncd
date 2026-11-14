package reconcile

import (
	"errors"
	"testing"
)

func TestParseDeleteLimit(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "off", want: "off"},
		{in: "none", want: "off"},
		{in: "0", want: "0"},
		{in: "250", want: "250"},
		{in: "50%", want: "50%"},
		{in: " 25 % ", want: "25%"},
		{in: "", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "101%", wantErr: true},
		{in: "lots", wantErr: true},
	} {
		got, err := ParseDeleteLimit(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDeleteLimit(%q): expected an error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDeleteLimit(%q): %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ParseDeleteLimit(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestDeleteLimitExceeded(t *testing.T) {
	mustParse := func(s string) DeleteLimit {
		l, err := ParseDeleteLimit(s)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	for _, tc := range []struct {
		limit          string
		deletes, total int
		want           bool
	}{
		{"off", 1_000_000, 1_000_000, false},
		{"0", 0, 100, false},
		{"0", 1, 100, true},
		{"100", 100, 100, false},
		{"100", 101, 5000, true},
		{"50%", 500, 1000, false},
		{"50%", 501, 1000, true},
		// A percentage never trips on a handful of files.
		{"50%", 3, 3, false},
		{"50%", 10, 10, false},
		{"50%", 11, 11, true},
		{"0%", 11, 1000, true},
	} {
		if got := mustParse(tc.limit).Exceeded(tc.deletes, tc.total); got != tc.want {
			t.Errorf("limit %s: Exceeded(%d, %d) = %v, want %v", tc.limit, tc.deletes, tc.total, got, tc.want)
		}
	}
	if !(DeleteLimit{}).Unlimited() {
		t.Error("zero DeleteLimit must be unlimited")
	}
}

func TestDeleteLimitCheck(t *testing.T) {
	l, _ := ParseDeleteLimit("5")
	if err := l.Check("remote", 5, 10); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	err := l.Check("remote", 6, 10)
	if !errors.Is(err, ErrDeleteLimit) {
		t.Fatalf("over the limit: got %v, want ErrDeleteLimit", err)
	}
}
