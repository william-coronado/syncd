package reconcile

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrDeleteLimit reports that a run was refused, before changing anything,
// because it planned more deletions on one side than its DeleteLimit allows.
// A mass delete is what almost every sync catastrophe looks like from the
// inside (a root that scans as empty, the wrong share behind a mount point),
// so an unattended run stops rather than carrying it out.
var ErrDeleteLimit = errors.New("delete limit exceeded")

// DefaultDeleteLimit is the --max-delete default.
const DefaultDeleteLimit = "50%"

// percentLimitFloor is the number of deletions a percentage limit always
// allows: without it, removing 2 files from a 3-file tree would trip "50%".
const percentLimitFloor = 10

type deleteLimitKind int

const (
	deleteUnlimited deleteLimitKind = iota // zero value: no limit
	deleteCount
	deletePercent
)

// DeleteLimit caps how many files one run may delete from one side. The
// zero value imposes no limit.
type DeleteLimit struct {
	kind deleteLimitKind
	n    int
}

// ParseDeleteLimit parses a --max-delete value: "off" (no limit), "N" (at
// most N deletions per side; 0 forbids deleting), or "N%" (at most N percent
// of the files known on that side - and never fewer than percentLimitFloor).
func ParseDeleteLimit(s string) (DeleteLimit, error) {
	s = strings.TrimSpace(s)
	bad := func() (DeleteLimit, error) {
		return DeleteLimit{}, fmt.Errorf("invalid delete limit %q (want a count like 100, a percentage like 50%%, or off)", s)
	}
	switch strings.ToLower(s) {
	case "off", "none", "unlimited":
		return DeleteLimit{}, nil
	case "":
		return bad()
	}
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(pct))
		if err != nil || n < 0 || n > 100 {
			return bad()
		}
		return DeleteLimit{kind: deletePercent, n: n}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return bad()
	}
	return DeleteLimit{kind: deleteCount, n: n}, nil
}

// Unlimited reports whether l imposes no limit.
func (l DeleteLimit) Unlimited() bool { return l.kind == deleteUnlimited }

// Exceeded reports whether deleting deletes of the total files known on a
// side is more than l allows.
func (l DeleteLimit) Exceeded(deletes, total int) bool {
	switch l.kind {
	case deleteCount:
		return deletes > l.n
	case deletePercent:
		return deletes > percentLimitFloor && deletes*100 > l.n*total
	}
	return false
}

func (l DeleteLimit) String() string {
	switch l.kind {
	case deleteCount:
		return strconv.Itoa(l.n)
	case deletePercent:
		return strconv.Itoa(l.n) + "%"
	}
	return "off"
}

// Check returns an ErrDeleteLimit error naming side if deleting deletes of
// total files there exceeds l, else nil.
func (l DeleteLimit) Check(side string, deletes, total int) error {
	if !l.Exceeded(deletes, total) {
		return nil
	}
	return fmt.Errorf("%w: this run would delete %d of %d file(s) from %s (--max-delete %s); nothing was changed",
		ErrDeleteLimit, deletes, total, side, l)
}
