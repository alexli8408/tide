# Tide

**Time-based autoscaling for Kubernetes.** Tide is a Kubernetes operator that
scales Deployments and StatefulSets up and down on recurring time windows —
for example, 5 replicas during business hours and 1 overnight.

> Work in progress. Full docs, architecture notes, and a quickstart are coming
> as the project lands.

## Why

Horizontal Pod Autoscalers react to load *after* it arrives. For workloads
with predictable traffic patterns (internal tools, batch consumers, staging
environments), scheduled scaling is simpler, cheaper, and more predictable:
capacity is there *before* the morning rush, and clusters aren't paying for
idle replicas overnight.

## Status

- [x] Project scaffolding
- [ ] `ScalingSchedule` CRD (v1alpha1)
- [ ] Schedule engine (time windows, timezones, midnight wrap)
- [ ] Reconciler with drift detection
- [ ] Unit + controller tests
- [ ] Manifests, CI, quickstart

## License

MIT
