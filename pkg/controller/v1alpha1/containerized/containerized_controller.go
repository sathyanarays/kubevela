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

package containerized

import (
	"context"
	"fmt"
	"reflect"

	cpv1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/oam-kubernetes-runtime/pkg/oam/util"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cloud-native-application/rudrx/api/v1alpha1"
)

// Reconcile error strings.
const (
	errRenderDeployment = "cannot render deployment"
	errRenderService    = "cannot render service"
	errApplyDeployment  = "cannot apply the deployment"
	errApplyService     = "cannot apply the service"
)

var (
	deploymentKind       = reflect.TypeOf(appsv1.Deployment{}).Name()
	deploymentAPIVersion = appsv1.SchemeGroupVersion.String()
	serviceKind          = reflect.TypeOf(corev1.Service{}).Name()
	serviceAPIVersion    = corev1.SchemeGroupVersion.String()
)

const (
	labelNameKey = "component.oam.dev/name"
)

// ContainerizedReconciler reconciles a Containerized object
type ContainerizedReconciler struct {
	client.Client
	log    logr.Logger
	record event.Recorder
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=standard.oam.dev,resources=containerizeds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=standard.oam.dev,resources=containerizeds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=services,verbs=get;list;watch;create;update;patch;delete
func (r *ContainerizedReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	ctx := context.Background()
	log := r.log.WithValues("containerized", req.NamespacedName)
	log.Info("Reconcile containerized workload")

	var workload v1alpha1.Containerized
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Containerized workload is deleted")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Get the workload", "apiVersion", workload.APIVersion, "kind", workload.Kind)
	// find the resource object to record the event to, default is the parent appConfig.
	eventObj, err := util.LocateParentAppConfig(ctx, r.Client, &workload)
	if eventObj == nil {
		// fallback to workload itself
		log.Error(err, "workload", "name", workload.Name)
		eventObj = &workload
	}
	deploy, err := r.renderDeployment(ctx, &workload)
	if err != nil {
		log.Error(err, "Failed to render a deployment")
		r.record.Event(eventObj, event.Warning(errRenderDeployment, err))
		return util.ReconcileWaitResult,
			util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errRenderDeployment)))
	}
	// merge patch
	applyOpts := []client.PatchOption{client.ForceOwnership, client.FieldOwner(workload.GetUID())}
	if err := r.Patch(ctx, deploy, client.Merge, applyOpts...); err != nil {
		log.Error(err, "Failed to apply to a deployment")
		r.record.Event(eventObj, event.Warning(errApplyDeployment, err))
		return util.ReconcileWaitResult,
			util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errApplyDeployment)))
	}
	r.record.Event(eventObj, event.Normal("Deployment created",
		fmt.Sprintf("Workload `%s` successfully patched a deployment `%s`",
			workload.Name, deploy.Name)))

	// create a service for the workload
	service, err := r.renderService(ctx, &workload)
	if err != nil {
		log.Error(err, "Failed to render a service")
		r.record.Event(eventObj, event.Warning(errRenderService, err))
		return util.ReconcileWaitResult,
			util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errRenderService)))
	}
	// merge apply the service
	if err := r.Patch(ctx, service, client.Merge, applyOpts...); err != nil {
		log.Error(err, "Failed to apply a service")
		r.record.Event(eventObj, event.Warning(errApplyDeployment, err))
		return util.ReconcileWaitResult,
			util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errApplyService)))
	}
	r.record.Event(eventObj, event.Normal("Service created",
		fmt.Sprintf("Workload `%s` successfully server side patched a service `%s`",
			workload.Name, service.Name)))

	// record the new deployment, new service
	workload.Status.Resources = []cpv1alpha1.TypedReference{
		{
			APIVersion: deploy.GetObjectKind().GroupVersionKind().GroupVersion().String(),
			Kind:       deploy.GetObjectKind().GroupVersionKind().Kind,
			Name:       deploy.GetName(),
			UID:        deploy.UID,
		},
		{
			APIVersion: service.GetObjectKind().GroupVersionKind().GroupVersion().String(),
			Kind:       service.GetObjectKind().GroupVersionKind().Kind,
			Name:       service.GetName(),
			UID:        service.UID,
		},
	}

	if err := r.Status().Update(ctx, &workload); err != nil {
		return util.ReconcileWaitResult, err
	}
	return ctrl.Result{}, util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileSuccess())
}

// create a corresponding deployment
func (r *ContainerizedReconciler) renderDeployment(ctx context.Context,
	workload *v1alpha1.Containerized) (*appsv1.Deployment, error) {
	// generate the deployment
	deploy := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       deploymentKind,
			APIVersion: deploymentAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workload.GetName(),
			Namespace: workload.GetNamespace(),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: workload.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					labelNameKey: workload.GetName(),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labelNameKey: workload.GetName(),
					},
				},
				Spec: workload.Spec.PodSpec,
			},
		},
	}
	// pass through label and annotation from the workload to the deployment
	util.PassLabelAndAnnotation(workload, deploy)
	// pass through label and annotation from the workload to the pod template too
	util.PassLabelAndAnnotation(workload, &deploy.Spec.Template)

	r.log.Info("rendered a deployment", "deploy", deploy.Spec.Template.Spec)

	// set the controller reference so that we can watch this deployment and it will be deleted automatically
	if err := ctrl.SetControllerReference(workload, deploy, r.Scheme); err != nil {
		return nil, err
	}

	return deploy, nil
}

// create a service for the deployment
func (r *ContainerizedReconciler) renderService(ctx context.Context,
	workload *v1alpha1.Containerized) (*corev1.Service, error) {
	// create a service for the workload
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       serviceKind,
			APIVersion: serviceAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workload.GetName(),
			Namespace: workload.GetNamespace(),
			Labels: map[string]string{
				labelNameKey: string(workload.GetName()),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				labelNameKey: workload.GetName(),
			},
			Ports: []corev1.ServicePort{},
			Type:  corev1.ServiceTypeClusterIP,
		},
	}
	// create a port for each ports in the all the containers
	var servicePort int32 = 8080
	for _, container := range workload.Spec.PodSpec.Containers {
		for _, port := range container.Ports {
			sp := corev1.ServicePort{
				Name:       port.Name,
				Protocol:   port.Protocol,
				Port:       servicePort,
				TargetPort: intstr.FromInt(int(port.ContainerPort)),
			}
			service.Spec.Ports = append(service.Spec.Ports, sp)
			servicePort++
		}
	}

	// always set the controller reference so that we can watch this service and
	if err := ctrl.SetControllerReference(workload, service, r.Scheme); err != nil {
		return nil, err
	}
	return service, nil
}

func (r *ContainerizedReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.record = event.NewAPIRecorder(mgr.GetEventRecorderFor("Containerized")).
		WithAnnotations("controller", "Containerized")
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Containerized{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// Setup adds a controller that reconciles MetricsTrait.
func Setup(mgr ctrl.Manager) error {
	reconciler := ContainerizedReconciler{
		Client: mgr.GetClient(),
		log:    ctrl.Log.WithName("Containerized"),
		Scheme: mgr.GetScheme(),
	}
	return reconciler.SetupWithManager(mgr)
}
