package panels

import (
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

// ThresholdStep is one absolute threshold color step (nil Value is the base color).
type ThresholdStep struct {
	Value *float64
	Color string
}

// AbsoluteThresholds builds Grafana absolute threshold config from ordered steps.
func AbsoluteThresholds(steps []ThresholdStep) *dashboard.ThresholdsConfigBuilder {
	thresholds := make([]dashboard.Threshold, len(steps))
	for i, step := range steps {
		thresholds[i] = dashboard.Threshold{
			Value: step.Value,
			Color: step.Color,
		}
	}
	return dashboard.NewThresholdsConfigBuilder().
		Mode(dashboard.ThresholdsModeAbsolute).
		Steps(thresholds)
}
