/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics are registered with controller-runtime's registry, so they are
// served from the manager's existing /metrics endpoint alongside the
// standard controller-runtime metrics (reconcile counts, queue depths).
var (
	scaleOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tide_scale_operations_total",
		Help: "Scale operations performed by Tide, labelled by schedule and direction (up/down).",
	}, []string{"namespace", "schedule", "direction"})

	desiredReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tide_desired_replicas",
		Help: "Replica count the schedule currently calls for.",
	}, []string{"namespace", "schedule"})

	scheduleReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tide_schedule_ready",
		Help: "1 if the schedule's Ready condition is true, else 0.",
	}, []string{"namespace", "schedule"})
)

func init() {
	metrics.Registry.MustRegister(scaleOperations, desiredReplicas, scheduleReady)
}

// recordDecision publishes the per-schedule gauges after a reconcile.
func recordDecision(namespace, name string, desired int32, ready bool) {
	desiredReplicas.WithLabelValues(namespace, name).Set(float64(desired))
	readyValue := 0.0
	if ready {
		readyValue = 1.0
	}
	scheduleReady.WithLabelValues(namespace, name).Set(readyValue)
}

// forgetSchedule drops a deleted schedule's gauge series so /metrics does
// not keep reporting stale values forever.
func forgetSchedule(namespace, name string) {
	desiredReplicas.DeleteLabelValues(namespace, name)
	scheduleReady.DeleteLabelValues(namespace, name)
}

// recordInvalid marks a schedule not-ready and retires its desired-replicas
// series: an invalid spec has no meaningful desired count.
func recordInvalid(namespace, name string) {
	desiredReplicas.DeleteLabelValues(namespace, name)
	scheduleReady.WithLabelValues(namespace, name).Set(0)
}

// recordScale counts one scale operation.
func recordScale(namespace, name string, from, to int32) {
	direction := "up"
	if to < from {
		direction = "down"
	}
	scaleOperations.WithLabelValues(namespace, name, direction).Inc()
}
