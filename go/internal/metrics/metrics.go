// Package metrics provides Prometheus and JSON instrumentation for eneverre.
// Create a Store with New, register it in the App, and all collectors
// (Go runtime, database, cameras, build info) are updated automatically on
// each scrape.
package metrics

import (
	"database/sql"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"eneverre/internal/camera"
	"eneverre/internal/media"
)

const namespace = "eneverre"

// CameraStatusFn returns the current snapshot of camera media states.
// Called once per scrape, holding the engine's read lock.
type CameraStatusFn func() []media.CameraStatus

// PrivacyStateFn returns the privacy state for a given camera id.
type PrivacyStateFn func(id string) bool

// CamerasFn returns the current camera set. Called once per scrape so counts
// reflect runtime create/delete rather than a snapshot captured at startup.
type CamerasFn func() []camera.Camera

// Store holds the Prometheus registry and all metric collectors. Create one
// with New, then wire it into the App so the /api/metrics endpoints serve it.
type Store struct {
	reg *prometheus.Registry

	db        *sql.DB
	camerasFn CamerasFn
	statusFn  CameraStatusFn
	privacyFn PrivacyStateFn
}

// New creates a Store, registers all collectors, and returns it. Pass nil for
// db to disable the database-stats collector (it then reports nothing rather
// than zero values). The camerasFn, statusFn and privacyFn closures are called
// on every scrape; they must be safe for concurrent use.
func New(db *sql.DB, version string, statusFn CameraStatusFn, privacyFn PrivacyStateFn, camerasFn CamerasFn) *Store {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())

	s := &Store{
		reg:       reg,
		db:        db,
		camerasFn: camerasFn,
		statusFn:  statusFn,
		privacyFn: privacyFn,
	}

	reg.MustRegister(newDBStatsCollector(s))
	reg.MustRegister(newCamerasCollector(s))

	b := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information about eneverre.",
	}, []string{"version"})
	b.WithLabelValues(version).Set(1)
	reg.MustRegister(b)

	return s
}

// PrometheusHandler returns an HTTP handler that serves the Prometheus text
// format on /api/metrics.
func (s *Store) PrometheusHandler() http.Handler {
	return promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Gather returns all gathered metric families for the JSON handler.
func (s *Store) Gather() ([]*dto.MetricFamily, error) {
	return s.reg.Gather()
}

// ---------------------------------------------------------------------------
// DB stats collector
// ---------------------------------------------------------------------------

type dbStatsCollector struct {
	store       *Store
	descOpen    *prometheus.Desc
	descInUse   *prometheus.Desc
	descIdle    *prometheus.Desc
	descWait    *prometheus.Desc
	descWaitDur *prometheus.Desc
}

func newDBStatsCollector(s *Store) *dbStatsCollector {
	return &dbStatsCollector{
		store: s,
		descOpen: prometheus.NewDesc(
			namespace+"_db_connections_open",
			"Number of open database connections.",
			nil, nil,
		),
		descInUse: prometheus.NewDesc(
			namespace+"_db_connections_in_use",
			"Number of database connections currently in use.",
			nil, nil,
		),
		descIdle: prometheus.NewDesc(
			namespace+"_db_connections_idle",
			"Number of idle database connections.",
			nil, nil,
		),
		descWait: prometheus.NewDesc(
			namespace+"_db_connection_wait_count_total",
			"Total number of database connection wait events.",
			nil, nil,
		),
		descWaitDur: prometheus.NewDesc(
			namespace+"_db_connection_wait_duration_seconds_total",
			"Total time spent waiting for a database connection, in seconds.",
			nil, nil,
		),
	}
}

func (c *dbStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descOpen
	ch <- c.descInUse
	ch <- c.descIdle
	ch <- c.descWait
	ch <- c.descWaitDur
}

func (c *dbStatsCollector) Collect(ch chan<- prometheus.Metric) {
	db := c.store.db
	if db == nil {
		return
	}
	stats := db.Stats()
	ch <- prometheus.MustNewConstMetric(c.descOpen, prometheus.GaugeValue, float64(stats.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.descInUse, prometheus.GaugeValue, float64(stats.InUse))
	ch <- prometheus.MustNewConstMetric(c.descIdle, prometheus.GaugeValue, float64(stats.Idle))
	ch <- prometheus.MustNewConstMetric(c.descWait, prometheus.CounterValue, float64(stats.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.descWaitDur, prometheus.CounterValue, stats.WaitDuration.Seconds())
}

// ---------------------------------------------------------------------------
// Camera collector
// ---------------------------------------------------------------------------

// camerasCollector emits only aggregate counts across all cameras. It
// deliberately carries no per-camera "id" label: the metrics endpoint answers
// "how many cameras are recording / connected / in privacy", not "what is
// camera X doing". This keeps the endpoint from being a per-camera surveillance
// map and lets it stay useful without leaking each camera's identity or state.
type camerasCollector struct {
	store         *Store
	descTotal     *prometheus.Desc
	descConnected *prometheus.Desc
	descMSEActive *prometheus.Desc
	descRecording *prometheus.Desc
	descPrivacy   *prometheus.Desc
}

func newCamerasCollector(s *Store) *camerasCollector {
	return &camerasCollector{
		store: s,
		descTotal: prometheus.NewDesc(
			namespace+"_cameras_total",
			"Total number of configured cameras.",
			nil, nil,
		),
		descConnected: prometheus.NewDesc(
			namespace+"_cameras_connected",
			"Number of cameras with an active RTSP connection.",
			nil, nil,
		),
		descMSEActive: prometheus.NewDesc(
			namespace+"_cameras_mse_active",
			"Number of cameras with an active live MSE broadcaster.",
			nil, nil,
		),
		descRecording: prometheus.NewDesc(
			namespace+"_cameras_recording",
			"Number of cameras currently recording to disk.",
			nil, nil,
		),
		descPrivacy: prometheus.NewDesc(
			namespace+"_cameras_privacy",
			"Number of cameras with privacy enabled.",
			nil, nil,
		),
	}
}

func (c *camerasCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descTotal
	ch <- c.descConnected
	ch <- c.descMSEActive
	ch <- c.descRecording
	ch <- c.descPrivacy
}

func (c *camerasCollector) Collect(ch chan<- prometheus.Metric) {
	cameras := c.store.camerasFn()
	ch <- prometheus.MustNewConstMetric(c.descTotal, prometheus.GaugeValue, float64(len(cameras)))

	statuses := c.store.statusFn()
	statusByID := make(map[string]media.CameraStatus, len(statuses))
	for _, s := range statuses {
		statusByID[s.ID] = s
	}

	var connected, mseActive, recording, privacy int
	for _, cam := range cameras {
		if st, ok := statusByID[cam.ID]; ok {
			if st.Connected {
				connected++
			}
			if st.MSEActive {
				mseActive++
			}
			if st.Recording {
				recording++
			}
		}
		if c.store.privacyFn != nil && c.store.privacyFn(cam.ID) {
			privacy++
		}
	}

	ch <- prometheus.MustNewConstMetric(c.descConnected, prometheus.GaugeValue, float64(connected))
	ch <- prometheus.MustNewConstMetric(c.descMSEActive, prometheus.GaugeValue, float64(mseActive))
	ch <- prometheus.MustNewConstMetric(c.descRecording, prometheus.GaugeValue, float64(recording))
	ch <- prometheus.MustNewConstMetric(c.descPrivacy, prometheus.GaugeValue, float64(privacy))
}
