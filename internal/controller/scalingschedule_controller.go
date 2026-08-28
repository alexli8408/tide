/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tidev1alpha1 "github.com/alexli8408/tide/api/v1alpha1"
	"github.com/alexli8408/tide/internal/schedule"
)

// targetIndexKey indexes ScalingSchedules by "<Kind>/<name>" of their target,
// so a change to a workload can be mapped back to the schedules watching it.
const targetIndexKey = ".spec.targetRef"

// requeueSlack is added to boundary requeues so the reconcile lands just
// after the transition instant rather than racing it.
const requeueSlack = 500 * time.Millisecond

// ScalingScheduleReconciler reconciles ScalingSchedule objects.
type ScalingScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Clock returns the current time; nil means time.Now. Injected by tests.
	Clock func() time.Time
}

func (r *ScalingScheduleReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// RBAC for our own CRD, the workloads we scale, and the events we emit.
// +kubebuilder:rbac:groups=tide.alexli8408.github.io,resources=scalingschedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=tide.alexli8408.github.io,resources=scalingschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one ScalingSchedule: evaluate the schedule at the current
// instant, scale the target if its replica count disagrees, publish status,
// and requeue itself for the next window boundary.
func (r *ScalingScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var sched tidev1alpha1.ScalingSchedule
	if err := r.Get(ctx, req.NamespacedName, &sched); err != nil {
		// Deleted schedules need no cleanup: Tide owns nothing. The target
		// keeps whatever replica count it last had, which is the least
		// surprising behaviour for an operator being removed. Only the
		// schedule's metric series are dropped.
		if apierrors.IsNotFound(err) {
			forgetSchedule(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	base := sched.DeepCopy()
	now := r.now()

	decision, err := schedule.Evaluate(&sched.Spec, now)
	if err != nil {
		// An invalid spec cannot be retried into validity; wait for an edit.
		// Decision fields are cleared so status never shows values computed
		// from a previous, valid generation of the spec.
		log.Error(err, "invalid schedule")
		sched.Status.DesiredReplicas = nil
		sched.Status.ActiveWindow = ""
		sched.Status.NextTransitionTime = nil
		r.warnOnce(&sched, tidev1alpha1.ReasonInvalidSchedule, err.Error())
		r.setReady(&sched, metav1.ConditionFalse, tidev1alpha1.ReasonInvalidSchedule, err.Error())
		recordInvalid(sched.Namespace, sched.Name)
		return ctrl.Result{}, r.patchStatus(ctx, &sched, base)
	}

	sched.Status.DesiredReplicas = ptr.To(decision.Replicas)
	sched.Status.ActiveWindow = decision.WindowName
	if decision.NextTransition.IsZero() {
		sched.Status.NextTransitionTime = nil
	} else {
		sched.Status.NextTransitionTime = &metav1.Time{Time: decision.NextTransition}
	}

	// Exactly one schedule may manage a given workload. When several claim
	// the same target they would revert each other's scaling forever, so the
	// oldest (ties broken by name) wins deterministically and the rest go
	// Ready=False without touching the target.
	winner, err := r.conflictWinner(ctx, &sched)
	if err != nil {
		return ctrl.Result{}, err
	}
	if winner != sched.Name {
		msg := fmt.Sprintf("target %s %q is already managed by older ScalingSchedule %q; this schedule is inactive",
			targetKind(&sched), sched.Spec.TargetRef.Name, winner)
		r.warnOnce(&sched, tidev1alpha1.ReasonConflictingTarget, msg)
		r.setReady(&sched, metav1.ConditionFalse, tidev1alpha1.ReasonConflictingTarget, msg)
		recordDecision(sched.Namespace, sched.Name, decision.Replicas, false)
		return r.resultFor(decision, now), r.patchStatus(ctx, &sched, base)
	}

	target, replicas, err := r.getTarget(ctx, &sched)
	if apierrors.IsNotFound(err) {
		// No requeue needed for this case alone: the workload watch enqueues
		// this schedule the moment its target appears.
		msg := fmt.Sprintf("target %s %q not found", targetKind(&sched), sched.Spec.TargetRef.Name)
		r.warnOnce(&sched, tidev1alpha1.ReasonTargetNotFound, msg)
		r.setReady(&sched, metav1.ConditionFalse, tidev1alpha1.ReasonTargetNotFound, msg)
		recordDecision(sched.Namespace, sched.Name, decision.Replicas, false)
		return r.resultFor(decision, now), r.patchStatus(ctx, &sched, base)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	current := int32(1) // apps/v1 defaults nil replicas to 1
	if *replicas != nil {
		current = **replicas
	}

	switch {
	case sched.Spec.Suspend:
		r.setReady(&sched, metav1.ConditionTrue, tidev1alpha1.ReasonSuspended,
			fmt.Sprintf("scaling suspended; schedule wants %d replicas, target has %d", decision.Replicas, current))

	case current != decision.Replicas:
		*replicas = ptr.To(decision.Replicas)
		if err := r.Update(ctx, target); err != nil {
			r.setReady(&sched, metav1.ConditionFalse, tidev1alpha1.ReasonScaleFailed, err.Error())
			recordDecision(sched.Namespace, sched.Name, decision.Replicas, false)
			// Best-effort status, then let the error drive a backoff retry.
			_ = r.patchStatus(ctx, &sched, base)
			return ctrl.Result{}, err
		}
		reason := reasonFor(decision)
		recordScale(sched.Namespace, sched.Name, current, decision.Replicas)
		r.Recorder.Eventf(&sched, corev1.EventTypeNormal, "Scaled",
			"scaled %s %q from %d to %d replicas (%s)",
			targetKind(&sched), sched.Spec.TargetRef.Name, current, decision.Replicas, reason)
		log.Info("scaled target", "kind", targetKind(&sched), "name", sched.Spec.TargetRef.Name,
			"from", current, "to", decision.Replicas, "window", decision.WindowName)
		sched.Status.LastScaleTime = &metav1.Time{Time: now}
		r.setReady(&sched, metav1.ConditionTrue, tidev1alpha1.ReasonReconciled, reason)

	default:
		r.setReady(&sched, metav1.ConditionTrue, tidev1alpha1.ReasonReconciled,
			fmt.Sprintf("target at %d replicas as scheduled", current))
	}

	recordDecision(sched.Namespace, sched.Name, decision.Replicas, true)
	return r.resultFor(decision, now), r.patchStatus(ctx, &sched, base)
}

// resultFor requeues the schedule for its next window boundary. Landing
// slightly after the boundary (requeueSlack) avoids evaluating a hair before
// the transition and sleeping another full cycle.
func (r *ScalingScheduleReconciler) resultFor(decision schedule.Decision, now time.Time) ctrl.Result {
	if decision.NextTransition.IsZero() {
		return ctrl.Result{}
	}
	delay := decision.NextTransition.Sub(now) + requeueSlack
	if delay < requeueSlack {
		delay = requeueSlack
	}
	return ctrl.Result{RequeueAfter: delay}
}

// getTarget fetches the schedule's workload and returns it together with a
// pointer to its replica field, so the caller can read and write replicas
// without caring which concrete type it holds.
func (r *ScalingScheduleReconciler) getTarget(ctx context.Context, sched *tidev1alpha1.ScalingSchedule) (client.Object, **int32, error) {
	key := types.NamespacedName{Namespace: sched.Namespace, Name: sched.Spec.TargetRef.Name}
	switch targetKind(sched) {
	case tidev1alpha1.TargetKindStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, sts); err != nil {
			return nil, nil, err
		}
		return sts, &sts.Spec.Replicas, nil
	default:
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, key, dep); err != nil {
			return nil, nil, err
		}
		return dep, &dep.Spec.Replicas, nil
	}
}

// targetKind returns the target's kind, defaulting to Deployment so the
// controller behaves sensibly even if CRD defaulting was bypassed.
func targetKind(sched *tidev1alpha1.ScalingSchedule) tidev1alpha1.TargetKind {
	if sched.Spec.TargetRef.Kind == "" {
		return tidev1alpha1.TargetKindDeployment
	}
	return sched.Spec.TargetRef.Kind
}

func reasonFor(decision schedule.Decision) string {
	switch {
	case decision.Held && decision.WindowName != "":
		return fmt.Sprintf("scale-down delay holding window %q replicas", decision.WindowName)
	case decision.Held:
		return "scale-down delay holding default replicas"
	case decision.WindowName == "":
		return "no active window, applying default replicas"
	default:
		return fmt.Sprintf("window %q active", decision.WindowName)
	}
}

// conflictWinner returns the name of the schedule entitled to manage this
// schedule's target: the oldest by creation time among all schedules in the
// namespace indexing the same target, ties broken by name.
func (r *ScalingScheduleReconciler) conflictWinner(ctx context.Context, sched *tidev1alpha1.ScalingSchedule) (string, error) {
	var claimants tidev1alpha1.ScalingScheduleList
	err := r.List(ctx, &claimants,
		client.InNamespace(sched.Namespace),
		client.MatchingFields{targetIndexKey: string(targetKind(sched)) + "/" + sched.Spec.TargetRef.Name})
	if err != nil {
		return "", fmt.Errorf("listing schedules for conflict check: %w", err)
	}
	winner := sched
	for i := range claimants.Items {
		c := &claimants.Items[i]
		older := c.CreationTimestamp.Time.Before(winner.CreationTimestamp.Time)
		tieButFirst := c.CreationTimestamp.Time.Equal(winner.CreationTimestamp.Time) && c.Name < winner.Name
		if older || tieButFirst {
			winner = c
		}
	}
	return winner.Name, nil
}

// warnOnce emits a Warning event only if it would change the Ready
// condition. Without this, the reconcile triggered by our own status patch
// would duplicate every warning.
func (r *ScalingScheduleReconciler) warnOnce(sched *tidev1alpha1.ScalingSchedule, reason, message string) {
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reason || cond.Message != message {
		r.Recorder.Event(sched, corev1.EventTypeWarning, reason, message)
	}
}

func (r *ScalingScheduleReconciler) setReady(sched *tidev1alpha1.ScalingSchedule, status metav1.ConditionStatus, reason, message string) {
	sched.Status.ObservedGeneration = sched.Generation
	meta.SetStatusCondition(&sched.Status.Conditions, metav1.Condition{
		Type:               tidev1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sched.Generation,
	})
}

// patchStatus writes status with a merge patch against the pre-reconcile
// object, so it cannot conflict with concurrent spec updates.
func (r *ScalingScheduleReconciler) patchStatus(ctx context.Context, sched, base *tidev1alpha1.ScalingSchedule) error {
	return r.Status().Patch(ctx, sched, client.MergeFrom(base))
}

// mapWorkload maps a changed Deployment/StatefulSet to the ScalingSchedules
// targeting it, via the field index. This is what makes Tide self-healing:
// a manual `kubectl scale` on a managed workload triggers a reconcile that
// immediately puts the scheduled replica count back.
func (r *ScalingScheduleReconciler) mapWorkload(kind tidev1alpha1.TargetKind) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var schedules tidev1alpha1.ScalingScheduleList
		err := r.List(ctx, &schedules,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{targetIndexKey: string(kind) + "/" + obj.GetName()})
		if err != nil {
			logf.FromContext(ctx).Error(err, "mapping workload to schedules")
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(schedules.Items))
		for _, sched := range schedules.Items {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: sched.Namespace, Name: sched.Name},
			})
		}
		return reqs
	}
}

// SetupWithManager registers the index, the primary watch, and the workload
// watches with the manager.
func (r *ScalingScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&tidev1alpha1.ScalingSchedule{}, targetIndexKey,
		func(obj client.Object) []string {
			sched := obj.(*tidev1alpha1.ScalingSchedule)
			return []string{string(targetKind(sched)) + "/" + sched.Spec.TargetRef.Name}
		})
	if err != nil {
		return fmt.Errorf("adding target index: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&tidev1alpha1.ScalingSchedule{}).
		Watches(&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkload(tidev1alpha1.TargetKindDeployment))).
		Watches(&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkload(tidev1alpha1.TargetKindStatefulSet))).
		Named("scalingschedule").
		Complete(r)
}
