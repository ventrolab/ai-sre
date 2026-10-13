package guard

import "fmt"

type Risk int

const (
	RiskLow Risk = iota + 1
	RiskMedium
	RiskHigh
)

func ParseRisk(s string) (Risk, error) {
	switch s {
	case "low":
		return RiskLow, nil
	case "medium":
		return RiskMedium, nil
	case "high":
		return RiskHigh, nil
	}
	return 0, fmt.Errorf("unknown risk %q (want low, medium or high)", s)
}

func (r Risk) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	}
	return "unknown"
}
