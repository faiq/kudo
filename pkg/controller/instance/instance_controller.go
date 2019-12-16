/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instance

import (
	"context"
	"fmt"
	"log"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/kudobuilder/kudo/pkg/apis/kudo/v1beta1"
	kudov1beta1 "github.com/kudobuilder/kudo/pkg/apis/kudo/v1beta1"
	"github.com/kudobuilder/kudo/pkg/apitools/instance"
	"github.com/kudobuilder/kudo/pkg/engine"
	"github.com/kudobuilder/kudo/pkg/engine/renderer"
	"github.com/kudobuilder/kudo/pkg/engine/task"
	"github.com/kudobuilder/kudo/pkg/engine/workflow"
	"github.com/kudobuilder/kudo/pkg/util/kudo"
)

// Reconciler reconciles an Instance object.
type Reconciler struct {
	client.Client
	Recorder record.EventRecorder
	Scheme   *runtime.Scheme
}

// SetupWithManager registers this reconciler with the controller manager
func (r *Reconciler) SetupWithManager(
	mgr ctrl.Manager) error {
	addOvRelatedInstancesToReconcile := handler.ToRequestsFunc(
		func(obj handler.MapObject) []reconcile.Request {
			requests := make([]reconcile.Request, 0)
			instances := &kudov1beta1.InstanceList{}
			// we are listing all instances here, which could come with some performance penalty
			// obj possible optimization is to introduce filtering based on operatorversion (or operator)
			err := mgr.GetClient().List(
				context.TODO(),
				instances,
			)
			if err != nil {
				log.Printf("InstanceController: Error fetching instances list for operator %v: %v", obj.Meta.GetName(), err)
				return nil
			}
			for _, inst := range instances.Items {
				// we need to pick only those instances, that belong to the OperatorVersion we're reconciling
				if inst.Spec.OperatorVersion.Name == obj.Meta.GetName() &&
					instance.OperatorVersionNamespace(&inst) == obj.Meta.GetNamespace() {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      inst.Name,
							Namespace: inst.Namespace,
						},
					})
				}
			}
			return requests
		})

	// hasAnnotation returns true if an annotation with the passed key is found in the map
	hasAnnotation := func(key string, annotations map[string]string) bool {
		if annotations == nil {
			return false
		}
		for k := range annotations {
			if k == key {
				return true
			}
		}
		return false
	}

	// resPredicate ignores DeleteEvents for pipe-pods only (marked with task.PipePodAnnotation). This is due to an
	// inherent race that was described in detail in #1116 (https://github.com/kudobuilder/kudo/issues/1116)
	// tl;dr: pipe task will delete the pipe pod at the end of the execution. this would normally trigger another
	// Instance reconciliation which might end up copying pipe files twice. we avoid this by explicitly ignoring
	// DeleteEvents for pipe-pods.
	resPredicate := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return !hasAnnotation(task.PipePodAnnotation, e.Meta.GetAnnotations()) },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kudov1beta1.Instance{}).
		Owns(&kudov1beta1.Instance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Pod{}).
		WithEventFilter(resPredicate).
		Watches(&source.Kind{Type: &kudov1beta1.OperatorVersion{}}, &handler.EnqueueRequestsFromMapFunc{ToRequests: addOvRelatedInstancesToReconcile}).
		Complete(r)
}

// Reconcile is the main controller method that gets called every time something about the instance changes
//
//   +-------------------------------+
//   | Query state of Instance       |
//   | and OperatorVersion           |
//   +-------------------------------+
//                  |
//                  v
//   +-------------------------------+
//   | Update finalizers if cleanup  |
//   | plan exists                   |
//   +-------------------------------+
//                  |
//                  v
//   +-------------------------------+
//   | Start new plan if required    |
//   | and none is running           |
//   +-------------------------------+
//                  |
//                  v
//   +-------------------------------+
//   | If there is plan in progress, |
//   | proceed with the execution    |
//   +-------------------------------+
//                  |
//                  v
//   +-------------------------------+
//   | Update instance with new      |
//   | state of the execution        |
//   +-------------------------------+
//
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
func (r *Reconciler) Reconcile(request ctrl.Request) (ctrl.Result, error) {
	// ---------- 1. Query the current state ----------

	log.Printf("InstanceController: Received Reconcile request for instance \"%+v\"", request.Name)

	op := newReconcileOperation(r.Client, r.Recorder)

	err := op.loadInstance(request)
	if err != nil {
		return reconcile.Result{}, err
	}
	err = op.loadOperatorVersion()
	if err != nil {
		if apierrors.IsNotFound(err) { // not retrying if instance not found, probably someone manually removed it?
			log.Printf("Instance %s was deleted, nothing to reconcile.", request.NamespacedName)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// ---------- 2. Check if the object is being deleted ----------
	if !instance.IsDeleting(op.instance) {
		if _, hasCleanupPlan := op.ov.Spec.Plans[kudov1beta1.CleanupPlanName]; hasCleanupPlan {
			if op.TryAddFinalizerToInstance() {
				log.Printf("InstanceController: Adding finalizer on instance %s/%s", op.instance.Namespace, op.instance.Name)
			}
		}
	} else {
		log.Printf("InstanceController: Instance %s/%s is being deleted", op.instance.Namespace, op.instance.Name)
	}

	// ---------- 3. Check if we should start execution of new plan ----------
	planToBeExecuted, err := op.GetPlanToBeExecuted()
	if err != nil {
		return reconcile.Result{}, err
	}
	if planToBeExecuted != nil {
		log.Printf("InstanceController: Going to start execution of plan %s on instance %s/%s", kudo.StringValue(planToBeExecuted), op.instance.Namespace, op.instance.Name)
		err = op.StartPlanExecution(kudo.StringValue(planToBeExecuted))
		if err != nil {
			return reconcile.Result{}, op.handleError(err)
		}
		r.Recorder.Event(op.instance, "Normal", "PlanStarted", fmt.Sprintf("Execution of plan %s started", kudo.StringValue(planToBeExecuted)))
	}

	// ---------- 4. If there's currently active plan, continue with the execution ----------

	activePlanStatus := instance.GetPlanInProgress(op.instance)
	if activePlanStatus == nil { // we have no plan in progress
		log.Printf("InstanceController: Nothing to do, no plan in progress for instance %s/%s", op.instance.Namespace, op.instance.Name)
		return reconcile.Result{}, nil
	}

	metadata := &engine.Metadata{
		OperatorVersionName: op.ov.Name,
		OperatorVersion:     op.ov.Spec.Version,
		AppVersion:          op.ov.Spec.AppVersion,
		ResourcesOwner:      op.instance,
		OperatorName:        op.ov.Spec.Operator.Name,
		InstanceNamespace:   op.instance.Namespace,
		InstanceName:        op.instance.Name,
	}

	activePlan, err := preparePlanExecution(op.instance, op.ov, activePlanStatus, metadata)
	if err != nil {
		err = op.handleError(err)
		return reconcile.Result{}, err
	}
	log.Printf("InstanceController: Going to proceed in execution of active plan %s on instance %s/%s", activePlan.Name, op.instance.Namespace, op.instance.Name)
	newStatus, err := workflow.Execute(activePlan, metadata, r.Client, &renderer.KustomizeEnhancer{Scheme: r.Scheme}, time.Now())

	// ---------- 5. Update status of instance after the execution proceeded ----------
	if newStatus != nil {
		instance.UpdateStatus(op.instance, newStatus)
	}
	if err != nil {
		err = op.handleError(err)
		return reconcile.Result{}, err
	}

	err = op.updateInstance()
	if err != nil {
		log.Printf("InstanceController: Error when updating instance. %v", err)
		return reconcile.Result{}, err
	}

	if op.instance.Status.AggregatedStatus.Status.IsTerminal() {
		r.Recorder.Event(op.instance, "Normal", "PlanFinished", fmt.Sprintf("Execution of plan %s finished with status %s", activePlanStatus.Name, op.instance.Status.AggregatedStatus.Status))
	}

	return reconcile.Result{}, nil
}

func preparePlanExecution(instance *kudov1beta1.Instance, ov *kudov1beta1.OperatorVersion, activePlanStatus *kudov1beta1.PlanStatus, meta *engine.Metadata) (*workflow.ActivePlan, error) {
	planSpec, ok := ov.Spec.Plans[activePlanStatus.Name]
	if !ok {
		return nil, &engine.ExecutionError{Err: fmt.Errorf("%wcould not find required plan: %v", engine.ErrFatalExecution, activePlanStatus.Name), EventName: "InvalidPlan"}
	}

	params := paramsMap(instance, ov)
	pipes, err := pipesMap(activePlanStatus.Name, &planSpec, ov.Spec.Tasks, meta)
	if err != nil {
		return nil, &engine.ExecutionError{Err: fmt.Errorf("%wcould not make task pipes: %v", engine.ErrFatalExecution, err), EventName: "InvalidPlan"}
	}

	return &workflow.ActivePlan{
		Name:       activePlanStatus.Name,
		Spec:       &planSpec,
		PlanStatus: activePlanStatus,
		Tasks:      ov.Spec.Tasks,
		Templates:  ov.Spec.Templates,
		Params:     params,
		Pipes:      pipes,
	}, nil
}

// paramsMap generates {{ Params.* }} map of keys and values which is later used during template rendering.
func paramsMap(instance *kudov1beta1.Instance, operatorVersion *kudov1beta1.OperatorVersion) map[string]string {
	params := make(map[string]string)

	for k, v := range instance.Spec.Parameters {
		params[k] = v
	}

	// Merge instance parameter overrides with operator version, if no override exist, use the default one
	for _, param := range operatorVersion.Spec.Parameters {
		if _, ok := params[param.Name]; !ok {
			params[param.Name] = kudo.StringValue(param.Default)
		}
	}

	return params
}

// pipesMap generates {{ Pipes.* }} map of keys and values which is later used during template rendering.
func pipesMap(planName string, plan *v1beta1.Plan, tasks []v1beta1.Task, emeta *engine.Metadata) (map[string]string, error) {
	taskByName := func(name string) (*v1beta1.Task, bool) {
		for _, t := range tasks {
			if t.Name == name {
				return &t, true
			}
		}
		return nil, false
	}

	pipes := make(map[string]string)

	for _, ph := range plan.Phases {
		for _, st := range ph.Steps {
			for _, tn := range st.Tasks {
				rmeta := renderer.Metadata{
					Metadata:  *emeta,
					PlanName:  planName,
					PhaseName: ph.Name,
					StepName:  st.Name,
					TaskName:  tn,
				}

				if t, ok := taskByName(tn); ok && t.Kind == task.PipeTaskKind {
					for _, pipe := range t.Spec.PipeTaskSpec.Pipe {
						if _, ok := pipes[pipe.Key]; ok {
							return nil, fmt.Errorf("duplicated pipe key %s", pipe.Key)
						}
						pipes[pipe.Key] = task.PipeArtifactName(rmeta, pipe.Key)
					}
				}
			}
		}
	}

	return pipes, nil
}

func parameterDiff(old, new map[string]string) map[string]string {
	diff := make(map[string]string)

	for key, val := range old {
		// If a parameter was removed in the new spec
		if _, ok := new[key]; !ok {
			diff[key] = val
		}
	}

	for key, val := range new {
		// If new spec parameter was added or changed
		if v, ok := old[key]; !ok || v != val {
			diff[key] = val
		}
	}

	return diff
}
