/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tidev1alpha1 "github.com/alexli8408/tide/api/v1alpha1"
)

// mondayNoon is Monday 2026-08-24 12:00 UTC, inside a Mon–Fri 09:00–17:00
// window. All tests freeze the reconciler's clock here unless noted.
var mondayNoon = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := tidev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testSchedule(mods ...func(*tidev1alpha1.ScalingSchedule)) *tidev1alpha1.ScalingSchedule {
	sched := &tidev1alpha1.ScalingSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "web-schedule", Namespace: "default", Generation: 1},
		Spec: tidev1alpha1.ScalingScheduleSpec{
			TargetRef:       tidev1alpha1.TargetRef{Kind: tidev1alpha1.TargetKindDeployment, Name: "web"},
			DefaultReplicas: 1,
			Windows: []tidev1alpha1.ScalingWindow{{
				Name:     "business-hours",
				Days:     []tidev1alpha1.DayOfWeek{"Mon", "Tue", "Wed", "Thu", "Fri"},
				Start:    "09:00",
				End:      "17:00",
				Replicas: 5,
			}},
		},
	}
	for _, mod := range mods {
		mod(sched)
	}
	return sched
}

func testDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
	}
}

func newReconciler(t *testing.T, now time.Time, objs ...client.Object) (*ScalingScheduleReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&tidev1alpha1.ScalingSchedule{}).
		WithIndex(&tidev1alpha1.ScalingSchedule{}, targetIndexKey, func(obj client.Object) []string {
			sched := obj.(*tidev1alpha1.ScalingSchedule)
			return []string{string(targetKind(sched)) + "/" + sched.Spec.TargetRef.Name}
		}).
		Build()
	recorder := record.NewFakeRecorder(16)
	return &ScalingScheduleReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: recorder,
		Clock:    func() time.Time { return now },
	}, recorder
}

func reconcileOnce(t *testing.T, r *ScalingScheduleReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "web-schedule"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result
}

func getDeployment(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web"}, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return dep
}

func getSchedule(t *testing.T, c client.Client) *tidev1alpha1.ScalingSchedule {
	t.Helper()
	sched := &tidev1alpha1.ScalingSchedule{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-schedule"}, sched); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	return sched
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestReconcileScalesUpInsideWindow(t *testing.T) {
	r, recorder := newReconciler(t, mondayNoon, testSchedule(), testDeployment(1))
	result := reconcileOnce(t, r)

	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 5 {
		t.Fatalf("want 5 replicas inside window, got %d", got)
	}

	sched := getSchedule(t, r.Client)
	if *sched.Status.DesiredReplicas != 5 || sched.Status.ActiveWindow != "business-hours" {
		t.Fatalf("status not updated: %+v", sched.Status)
	}
	if sched.Status.LastScaleTime == nil || !sched.Status.LastScaleTime.Time.Equal(mondayNoon) {
		t.Fatalf("want LastScaleTime %v, got %v", mondayNoon, sched.Status.LastScaleTime)
	}
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tidev1alpha1.ReasonReconciled {
		t.Fatalf("want Ready=True/Reconciled, got %+v", cond)
	}

	// The window ends 17:00; the requeue must land just after it.
	if want := 5*time.Hour + requeueSlack; result.RequeueAfter != want {
		t.Fatalf("want requeue after %v, got %v", want, result.RequeueAfter)
	}

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.Contains(events[0], "from 1 to 5") {
		t.Fatalf("want one Scaled event mentioning 1->5, got %v", events)
	}
}

func TestReconcileScalesDownOutsideWindow(t *testing.T) {
	saturday := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	r, _ := newReconciler(t, saturday, testSchedule(), testDeployment(5))
	reconcileOnce(t, r)

	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 1 {
		t.Fatalf("want default 1 replica outside window, got %d", got)
	}
	if sched := getSchedule(t, r.Client); sched.Status.ActiveWindow != "" {
		t.Fatalf("want no active window, got %q", sched.Status.ActiveWindow)
	}
}

func TestReconcileNoopWhenAtDesired(t *testing.T) {
	r, recorder := newReconciler(t, mondayNoon, testSchedule(), testDeployment(5))
	reconcileOnce(t, r)

	sched := getSchedule(t, r.Client)
	if sched.Status.LastScaleTime != nil {
		t.Fatalf("no scale happened, LastScaleTime must stay nil, got %v", sched.Status.LastScaleTime)
	}
	if events := drainEvents(recorder); len(events) != 0 {
		t.Fatalf("want no events for a no-op, got %v", events)
	}
}

func TestReconcileRevertsManualDrift(t *testing.T) {
	// Someone kubectl-scaled the deployment to 10 mid-window.
	r, _ := newReconciler(t, mondayNoon, testSchedule(), testDeployment(10))
	reconcileOnce(t, r)

	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 5 {
		t.Fatalf("manual drift must be reverted to 5, got %d", got)
	}
}

func TestReconcileSuspendLeavesTargetAlone(t *testing.T) {
	suspended := testSchedule(func(s *tidev1alpha1.ScalingSchedule) { s.Spec.Suspend = true })
	r, recorder := newReconciler(t, mondayNoon, suspended, testDeployment(2))
	reconcileOnce(t, r)

	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 2 {
		t.Fatalf("suspended schedule must not scale, got %d", got)
	}
	sched := getSchedule(t, r.Client)
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tidev1alpha1.ReasonSuspended {
		t.Fatalf("want Ready=True/Suspended, got %+v", cond)
	}
	// Status still reports what the schedule would do.
	if *sched.Status.DesiredReplicas != 5 {
		t.Fatalf("suspended status must still show desired=5, got %d", *sched.Status.DesiredReplicas)
	}
	if events := drainEvents(recorder); len(events) != 0 {
		t.Fatalf("want no events while suspended, got %v", events)
	}
}

func TestReconcileTargetNotFound(t *testing.T) {
	r, recorder := newReconciler(t, mondayNoon, testSchedule())
	result := reconcileOnce(t, r)

	sched := getSchedule(t, r.Client)
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tidev1alpha1.ReasonTargetNotFound {
		t.Fatalf("want Ready=False/TargetNotFound, got %+v", cond)
	}
	// Still requeued for the boundary so status stays fresh.
	if result.RequeueAfter == 0 {
		t.Fatal("want boundary requeue even without a target")
	}
	if events := drainEvents(recorder); len(events) != 1 || !strings.Contains(events[0], "TargetNotFound") {
		t.Fatalf("want a TargetNotFound warning event, got %v", events)
	}
}

func TestReconcileInvalidSpec(t *testing.T) {
	invalid := testSchedule(func(s *tidev1alpha1.ScalingSchedule) { s.Spec.TimeZone = "Mars/Olympus" })
	r, _ := newReconciler(t, mondayNoon, invalid, testDeployment(1))
	result := reconcileOnce(t, r)

	// Invalid specs are not retried: no requeue, wait for an edit.
	if result.RequeueAfter != 0 {
		t.Fatalf("invalid spec must not requeue, got %v", result.RequeueAfter)
	}
	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 1 {
		t.Fatalf("invalid spec must not scale, got %d", got)
	}
	sched := getSchedule(t, r.Client)
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tidev1alpha1.ReasonInvalidSchedule {
		t.Fatalf("want Ready=False/InvalidSchedule, got %+v", cond)
	}
}

func TestReconcileConflictingSchedulesElectOldest(t *testing.T) {
	older := testSchedule(func(s *tidev1alpha1.ScalingSchedule) {
		s.Name = "older-schedule"
		s.CreationTimestamp = metav1.NewTime(mondayNoon.Add(-2 * time.Hour))
	})
	newer := testSchedule(func(s *tidev1alpha1.ScalingSchedule) {
		s.Name = "web-schedule"
		s.CreationTimestamp = metav1.NewTime(mondayNoon.Add(-1 * time.Hour))
	})
	r, recorder := newReconciler(t, mondayNoon, older, newer, testDeployment(1))

	// Reconciling the newer schedule must not touch the deployment.
	reconcileOnce(t, r)
	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 1 {
		t.Fatalf("losing schedule must not scale, got %d replicas", got)
	}
	sched := getSchedule(t, r.Client)
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tidev1alpha1.ReasonConflictingTarget {
		t.Fatalf("want Ready=False/ConflictingTarget on the loser, got %+v", cond)
	}
	if events := drainEvents(recorder); len(events) != 1 || !strings.Contains(events[0], "older-schedule") {
		t.Fatalf("want one conflict warning naming the winner, got %v", events)
	}

	// A second pass (e.g. self-triggered by the status patch) must not
	// duplicate the warning event.
	reconcileOnce(t, r)
	if events := drainEvents(recorder); len(events) != 0 {
		t.Fatalf("conflict warning must not repeat, got %v", events)
	}

	// The winner still scales normally.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "older-schedule"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := *getDeployment(t, r.Client).Spec.Replicas; got != 5 {
		t.Fatalf("winning schedule must scale to 5, got %d", got)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("winner must keep its boundary requeue")
	}
}

func TestReconcileConflictTieBrokenByName(t *testing.T) {
	ts := metav1.NewTime(mondayNoon.Add(-1 * time.Hour))
	first := testSchedule(func(s *tidev1alpha1.ScalingSchedule) {
		s.Name = "a-schedule"
		s.CreationTimestamp = ts
	})
	second := testSchedule(func(s *tidev1alpha1.ScalingSchedule) {
		s.Name = "web-schedule"
		s.CreationTimestamp = ts
	})
	r, _ := newReconciler(t, mondayNoon, first, second, testDeployment(1))

	reconcileOnce(t, r) // reconciles "web-schedule", which loses the tie
	sched := getSchedule(t, r.Client)
	cond := meta.FindStatusCondition(sched.Status.Conditions, tidev1alpha1.ConditionReady)
	if cond == nil || cond.Reason != tidev1alpha1.ReasonConflictingTarget || !strings.Contains(cond.Message, "a-schedule") {
		t.Fatalf("equal timestamps must elect the lexicographically first name, got %+v", cond)
	}
}

func TestReconcileInvalidSpecClearsDecisionFields(t *testing.T) {
	r, _ := newReconciler(t, mondayNoon, testSchedule(), testDeployment(1))
	reconcileOnce(t, r)
	if sched := getSchedule(t, r.Client); sched.Status.DesiredReplicas == nil {
		t.Fatal("precondition: valid reconcile must populate status")
	}

	// Break the spec and reconcile again: stale decision fields must clear.
	sched := getSchedule(t, r.Client)
	sched.Spec.TimeZone = "Mars/Olympus"
	if err := r.Update(context.Background(), sched); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r)

	sched = getSchedule(t, r.Client)
	if sched.Status.DesiredReplicas != nil || sched.Status.ActiveWindow != "" || sched.Status.NextTransitionTime != nil {
		t.Fatalf("invalid spec must clear decision fields, got %+v", sched.Status)
	}
}

func TestReconcileTargetNotFoundWarnsOnce(t *testing.T) {
	r, recorder := newReconciler(t, mondayNoon, testSchedule())
	reconcileOnce(t, r)
	reconcileOnce(t, r)
	if events := drainEvents(recorder); len(events) != 1 {
		t.Fatalf("repeated reconciles with an unchanged condition must emit one warning, got %v", events)
	}
}

func TestReconcileStatefulSetTarget(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	sched := testSchedule(func(s *tidev1alpha1.ScalingSchedule) {
		s.Spec.TargetRef = tidev1alpha1.TargetRef{Kind: tidev1alpha1.TargetKindStatefulSet, Name: "db"}
	})
	r, _ := newReconciler(t, mondayNoon, sched, sts)
	reconcileOnce(t, r)

	got := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "db"}, got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 5 {
		t.Fatalf("want statefulset scaled to 5, got %d", *got.Spec.Replicas)
	}
}

func TestReconcileDeletedScheduleIsANoop(t *testing.T) {
	r, _ := newReconciler(t, mondayNoon)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "gone"},
	})
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("deleted schedule must reconcile cleanly, got %v, %v", result, err)
	}
}

func TestMapWorkloadFindsTargetingSchedules(t *testing.T) {
	other := testSchedule(func(s *tidev1alpha1.ScalingSchedule) {
		s.Name = "other-schedule"
		s.Spec.TargetRef.Name = "api"
	})
	r, _ := newReconciler(t, mondayNoon, testSchedule(), other, testDeployment(1))

	reqs := r.mapWorkload(tidev1alpha1.TargetKindDeployment)(context.Background(), testDeployment(1))
	if len(reqs) != 1 || reqs[0].Name != "web-schedule" {
		t.Fatalf("want exactly web-schedule mapped for deployment web, got %v", reqs)
	}

	// A StatefulSet named "web" must not map to a Deployment-targeting schedule.
	reqs = r.mapWorkload(tidev1alpha1.TargetKindStatefulSet)(context.Background(), &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
	})
	if len(reqs) != 0 {
		t.Fatalf("kind mismatch must not map, got %v", reqs)
	}
}
