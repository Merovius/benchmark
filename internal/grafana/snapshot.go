package grafana

import (
	"fmt"
	"math"
	"regexp"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"

	"github.com/prometheus/client_golang/api/prometheus"
	"github.com/prometheus/common/model"
)

const addr = "https://snapshot.raintank.io/api/snapshots"

type snapshotData struct {
	Target     string          `json:"target"`
	Datapoints [][]interface{} `json:"datapoints"`
	// Metric is a set of labels (e.g. instance=alp) which is retained
	// so that we can replace labels according to target.legendFormat.
	Metric model.Metric `json:"-"`
}

func datapointsQuery(api prometheus.QueryAPI, query string, queryRange prometheus.Range) ([]snapshotData, error) {
	val, err := api.QueryRange(context.Background(), query, queryRange)
	if err != nil {
		return nil, err
	}
	if val.Type() != model.ValMatrix {
		return nil, fmt.Errorf("Unexpected value type: got %q, want %q", val.Type(), model.ValMatrix)
	}
	matrix, ok := val.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("Bug: val.Type() == model.ValMatrix, but type assertion failed")
	}

	results := make([]snapshotData, matrix.Len())
	for idx, stream := range matrix {
		datapoints := make([][]interface{}, len(stream.Values))
		for idx, samplepair := range stream.Values {
			if math.IsNaN(float64(samplepair.Value)) {
				datapoints[idx] = []interface{}{nil, float64(samplepair.Timestamp)}
			} else {
				datapoints[idx] = []interface{}{float64(samplepair.Value), float64(samplepair.Timestamp)}
			}
		}

		results[idx] = snapshotData{
			Metric:     stream.Metric,
			Datapoints: datapoints,
		}
	}

	return results, nil
}

var aliasRe = regexp.MustCompile(`{{\s*(.+?)\s*}}`)

// renderTemplate is a re-implementation of renderTemplate in
// Grafana’s Prometheus datasource; for the original, see:
// https://github.com/grafana/grafana/blob/79138e211fac98bf1d12f1645ecd9fab5846f4fb/public/app/plugins/datasource/prometheus/datasource.ts#L263
func renderTemplate(format string, metric model.Metric) string {
	return aliasRe.ReplaceAllStringFunc(format, func(match string) string {
		matches := aliasRe.FindStringSubmatch(match)
		return string(metric[model.LabelName(matches[1])])
	})
}

// Snapshot snapshots a dashboard (as untyped JSON, i.e. interface{})
// by querying data in the specified interval from Prometheus.
func Snapshot(rdashboard interface{}, queryAPI prometheus.QueryAPI, start, end time.Time) (interface{}, error) {
	var (
		g       errgroup.Group
		panelMu sync.Mutex
	)
	dashboard := rdashboard.(map[string]interface{})
	for _, rrow := range dashboard["rows"].([]interface{}) {
		row := rrow.(map[string]interface{})
		for _, rpanel := range row["panels"].([]interface{}) {
			panel := rpanel.(map[string]interface{})
			if panel["targets"] == nil {
				continue
			}
			// queryAPI is safe to use in multiple goroutines.
			g.Go(func() error {
				var snapshotData []interface{}
				for _, rtarget := range panel["targets"].([]interface{}) {
					target := rtarget.(map[string]interface{})
					// Calculate “step” like Grafana. For the original code, see:
					// https://github.com/grafana/grafana/blob/79138e211fac98bf1d12f1645ecd9fab5846f4fb/public/app/plugins/datasource/prometheus/datasource.ts#L83
					intervalFactor := float64(1)
					if target["intervalFactor"] != nil {
						intervalFactor = target["intervalFactor"].(float64)
					}
					queryRange := end.Sub(start).Seconds()
					maxDataPoints := float64(500)
					step := math.Ceil((queryRange / maxDataPoints) * intervalFactor)
					if queryRange/step > 11000 {
						step = math.Ceil(queryRange / 11000)
					}
					datapoints, err := datapointsQuery(queryAPI, target["expr"].(string), prometheus.Range{
						Start: start,
						End:   end,
						Step:  time.Duration(step) * time.Second,
					})
					if err != nil {
						return err
					}
					for idx, dp := range datapoints {
						if target["legendFormat"] != nil && target["legendFormat"].(string) != "" {
							dp.Target = renderTemplate(target["legendFormat"].(string), dp.Metric)
						} else {
							dp.Target = dp.Metric.String()
						}
						datapoints[idx] = dp
						snapshotData = append(snapshotData, dp)
					}
				}
				// TODO: remove this mutex if the race detector says that’s okay
				panelMu.Lock()
				defer panelMu.Unlock()
				panel["snapshotData"] = snapshotData
				panel["targets"] = []interface{}{}
				panel["links"] = []interface{}{}
				panel["datasource"] = []interface{}{}
				return nil
			})
		}
	}

	// Set the time range so that the browser displays exactly the data we snapshotted.
	t := dashboard["time"].(map[string]interface{})
	t["from"] = start.Format(time.RFC3339)
	t["to"] = end.Format(time.RFC3339)

	return dashboard, g.Wait()
}
