// Package prediction implements the domain rules for what can be bet on and
// how a pick is validated — independently from the DB-layer CHECK
// constraint/functions added in the redesign_prediction_games migration and
// from the UI's own validation. All three must agree by design (that's the
// "triple layer of checks"), but none of them is allowed to trust the
// others: this file re-derives everything from first principles rather than
// calling out to the database to ask "is this valid."
package prediction

import "fmt"

type Type string

const (
	OpenSingle      Type = "OPEN_SINGLE"
	CloseSingle     Type = "CLOSE_SINGLE"
	Jodi            Type = "JODI"
	OpenSinglePana  Type = "OPEN_SINGLE_PANA"
	CloseSinglePana Type = "CLOSE_SINGLE_PANA"
	OpenDoublePana  Type = "OPEN_DOUBLE_PANA"
	CloseDoublePana Type = "CLOSE_DOUBLE_PANA"
	OpenTriplePana  Type = "OPEN_TRIPLE_PANA"
	CloseTriplePana Type = "CLOSE_TRIPLE_PANA"
	HalfSangam      Type = "HALF_SANGAM"
	FullSangam      Type = "FULL_SANGAM"
)

var allTypes = map[Type]bool{
	OpenSingle: true, CloseSingle: true, Jodi: true,
	OpenSinglePana: true, CloseSinglePana: true,
	OpenDoublePana: true, CloseDoublePana: true,
	OpenTriplePana: true, CloseTriplePana: true,
	HalfSangam: true, FullSangam: true,
}

func IsValid(t Type) bool { return allTypes[t] }

// AllTypes lists the 11 in a stable order — used wherever every type needs
// enumerating (e.g. computing per-type cutoffs for the Predict UI).
var AllTypes = []Type{
	OpenSingle, CloseSingle, Jodi,
	OpenSinglePana, CloseSinglePana,
	OpenDoublePana, CloseDoublePana,
	OpenTriplePana, CloseTriplePana,
	HalfSangam, FullSangam,
}

// CutoffGroup is "open" or "close" — which of the Round's two timestamps
// (minus the one-minute buffer) gates this type. Every CLOSE_* type is
// close-cutoff; everything else — including JODI and both Sangams, despite
// having no open/close split of their own — is open-cutoff. Explicit
// product decision (see ARCHITECTURE.md), not an oversight.
func (t Type) CutoffGroup() string {
	switch t {
	case CloseSingle, CloseSinglePana, CloseDoublePana, CloseTriplePana:
		return "close"
	default:
		return "open"
	}
}

// OddsFamily is the BetType (see rates.bet_type) whose rate applies. Every
// OPEN_*/CLOSE_* pair shares one family — this type only ever decides
// cutoff timing and pickedNumber format, never which Rate row is read.
func (t Type) OddsFamily() (string, bool) {
	switch t {
	case OpenSingle, CloseSingle:
		return "SINGLE", true
	case Jodi:
		return "JODI", true
	case OpenSinglePana, CloseSinglePana:
		return "SINGLE_PANA", true
	case OpenDoublePana, CloseDoublePana:
		return "DOUBLE_PANA", true
	case OpenTriplePana, CloseTriplePana:
		return "TRIPLE_PANA", true
	case HalfSangam:
		return "HALF_SANGAM", true
	case FullSangam:
		return "FULL_SANGAM", true
	default:
		return "", false
	}
}

// panaDigitValue: '0' sorts as if it were 10 — the domain's own ordering
// rule, not a general-purpose one.
func panaDigitValue(c byte) int {
	if c == '0' {
		return 10
	}
	return int(c - '0')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isValidPana: 3 digits, non-decreasing under panaDigitValue. Structural
// validity only — does not distinguish single/double/triple, since those
// are mutually exclusive by repeat pattern and each price differently (see
// isSinglePana/isDoublePana/isTriplePana below).
func isValidPana(s string) bool {
	if len(s) != 3 || !isDigit(s[0]) || !isDigit(s[1]) || !isDigit(s[2]) {
		return false
	}
	a, b, c := panaDigitValue(s[0]), panaDigitValue(s[1]), panaDigitValue(s[2])
	return a <= b && b <= c
}

func isSinglePana(s string) bool {
	return isValidPana(s) && s[0] != s[1] && s[1] != s[2] && s[0] != s[2]
}

func isDoublePana(s string) bool {
	if !isValidPana(s) {
		return false
	}
	allDistinct := s[0] != s[1] && s[1] != s[2] && s[0] != s[2]
	allSame := s[0] == s[1] && s[1] == s[2]
	return !allDistinct && !allSame
}

func isTriplePana(s string) bool {
	return isValidPana(s) && s[0] == s[1] && s[1] == s[2]
}

// ValidatePickedNumber enforces the format table from ARCHITECTURE.md:
//
//	OPEN_SINGLE/CLOSE_SINGLE   1 digit
//	JODI                       2 digits, "00".."99"
//	*_SINGLE_PANA              3-digit pana, all 3 digits distinct
//	*_DOUBLE_PANA              3-digit pana, exactly 2 digits equal
//	*_TRIPLE_PANA              3-digit pana, all 3 digits equal
//	HALF_SANGAM                "D-PPP" or "PPP-D"
//	FULL_SANGAM                "PPP-PPP" (open pana - close pana)
func ValidatePickedNumber(t Type, picked string) error {
	switch t {
	case OpenSingle, CloseSingle:
		if len(picked) == 1 && isDigit(picked[0]) {
			return nil
		}
		return fmt.Errorf("%s must be a single digit 0-9", t)

	case Jodi:
		if len(picked) == 2 && isDigit(picked[0]) && isDigit(picked[1]) {
			return nil
		}
		return fmt.Errorf("JODI must be two digits, 00-99")

	case OpenSinglePana, CloseSinglePana:
		if isSinglePana(picked) {
			return nil
		}
		return fmt.Errorf("%s must be a non-decreasing 3-digit pana with all distinct digits (0 sorts as 10)", t)

	case OpenDoublePana, CloseDoublePana:
		if isDoublePana(picked) {
			return nil
		}
		return fmt.Errorf("%s must be a non-decreasing 3-digit pana with exactly two equal digits", t)

	case OpenTriplePana, CloseTriplePana:
		if isTriplePana(picked) {
			return nil
		}
		return fmt.Errorf("%s must be a 3-digit pana with all three digits equal", t)

	case HalfSangam:
		if n := len(picked); n >= 5 {
			if idx := indexOfDash(picked); idx >= 0 {
				left, right := picked[:idx], picked[idx+1:]
				if len(left) == 1 && isDigit(left[0]) && isValidPana(right) {
					return nil
				}
				if isValidPana(left) && len(right) == 1 && isDigit(right[0]) {
					return nil
				}
			}
		}
		return fmt.Errorf("HALF_SANGAM must be \"D-PPP\" or \"PPP-D\" (a digit and a valid pana)")

	case FullSangam:
		if idx := indexOfDash(picked); idx >= 0 {
			left, right := picked[:idx], picked[idx+1:]
			if isValidPana(left) && isValidPana(right) {
				return nil
			}
		}
		return fmt.Errorf("FULL_SANGAM must be \"PPP-PPP\" (a valid open pana - a valid close pana)")

	default:
		return fmt.Errorf("unknown prediction type %s", t)
	}
}

func indexOfDash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}
