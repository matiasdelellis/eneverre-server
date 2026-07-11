package metrics

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// JSONHandler returns an HTTP handler that serves all metrics as a structured
// JSON object on /api/metrics/json. Each metric family is keyed by its name.
// Unlabeled metrics appear as a simple number; labeled metrics appear as an
// array of {labels: {...}, value: N} objects.
func (s *Store) JSONHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		families, err := s.Gather()
		if err != nil {
			http.Error(w, `{"error":"gather failed"}`, http.StatusInternalServerError)
			return
		}

		out := make(map[string]any, len(families))
		for _, f := range families {
			out[f.GetName()] = familyToJSON(f)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

type jsonMetric struct {
	Help  string `json:"help"`
	Type  string `json:"type"`
	Value any    `json:"value"` // float64 for unlabeled, []labeledMetric for labeled
}

type labeledMetric struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

func familyToJSON(f *dto.MetricFamily) jsonMetric {
	t := f.GetType().String()
	help := f.GetHelp()
	metrics := f.GetMetric()

	if len(metrics) == 0 {
		return jsonMetric{Help: help, Type: t, Value: nil}
	}

	// Check if the metric has labels
	hasLabels := len(metrics[0].GetLabel()) > 0

	if !hasLabels && len(metrics) == 1 {
		return jsonMetric{Help: help, Type: t, Value: extractValue(metrics[0], f.GetType())}
	}

	// Labeled metric or multiple unlabeled (e.g. histogram buckets)
	vals := make([]labeledMetric, 0, len(metrics))
	for _, m := range metrics {
		lm := labeledMetric{
			Labels: make(map[string]string, len(m.GetLabel())),
			Value:  extractValue(m, f.GetType()),
		}
		for _, l := range m.GetLabel() {
			lm.Labels[l.GetName()] = l.GetValue()
		}
		vals = append(vals, lm)
	}
	sort.Slice(vals, func(i, j int) bool {
		return labelKey(vals[i].Labels) < labelKey(vals[j].Labels)
	})
	return jsonMetric{Help: help, Type: t, Value: vals}
}

// labelKey builds a stable ordering key from a metric's labels so JSON output
// is deterministic across scrapes regardless of Go's map iteration order.
func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(',')
	}
	return b.String()
}

func extractValue(m *dto.Metric, t dto.MetricType) float64 {
	switch t {
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue()
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue()
	case dto.MetricType_SUMMARY:
		return m.GetSummary().GetSampleSum()
	case dto.MetricType_HISTOGRAM:
		return m.GetHistogram().GetSampleSum()
	default:
		return 0
	}
}
