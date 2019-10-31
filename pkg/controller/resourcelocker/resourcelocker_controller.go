package resourcelocker

import (
	"context"
	"encoding/json"
	errs "errors"
	"os"
	"reflect"
	"text/template"

	"github.com/redhat-cop/operator-utils/pkg/util"
	"github.com/redhat-cop/resource-locker-operator/pkg/apis/redhatcop/v1alpha1"
	redhatcopv1alpha1 "github.com/redhat-cop/resource-locker-operator/pkg/apis/redhatcop/v1alpha1"
	"github.com/redhat-cop/resource-locker-operator/pkg/lockedresourcecontroller"
	"github.com/redhat-cop/resource-locker-operator/pkg/patchlocker"
	"github.com/redhat-cop/resource-locker-operator/pkg/stoppablemanager"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"
)

const controllerName = "resourcelocker-controller"

var log = logf.Log.WithName(controllerName)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new ResourceLocker Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileResourceLocker{
		ReconcilerBase:  util.NewReconcilerBase(mgr.GetClient(), mgr.GetScheme(), mgr.GetConfig(), mgr.GetEventRecorderFor(controllerName)),
		ResourceLockers: map[string]ResourceLocker{},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ResourceLocker
	err = c.Watch(&source.Kind{Type: &redhatcopv1alpha1.ResourceLocker{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileResourceLocker implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileResourceLocker{}

// ReconcileResourceLocker reconciles a ResourceLocker object
type ReconcileResourceLocker struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	util.ReconcilerBase
	ResourceLockers map[string]ResourceLocker
}

// Reconcile reads that state of the cluster for a ResourceLocker object and makes changes based on the state read
// and what is in the ResourceLocker.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileResourceLocker) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ResourceLocker")

	// Fetch the ResourceLocker instance
	instance := &redhatcopv1alpha1.ResourceLocker{}
	err := r.GetClient().Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	//TODO manage lifecycle

	//First we build the resource locker corresponding to this instance
	newResourceLocker, err := r.getResourceLockerFromInstance(instance)
	if err != nil {
		reqLogger.Error(err, "Unable to create needed controllers")
		return reconcile.Result{}, err
	}

	//then we check if we have already started it before
	oldResourceLocker, ok := r.ResourceLockers[getKeyFromInstance(instance)]

	if ok {
		// we check if the content of the resource locker is the same
		if reflect.DeepEqual(newResourceLocker.LockedResources, oldResourceLocker.LockedResources) && reflect.DeepEqual(newResourceLocker.Patches, oldResourceLocker.Patches) {
			// nothing changed, we can return
			return reconcile.Result{}, nil
		}
		// here something changed and the oldResourceLocker is still running, so we need to stop it.
		oldResourceLocker.Manager.Stop()
	}
	// if we get here we need to put the newResourceLocker in the map
	r.ResourceLockers[getKeyFromInstance(instance)] = *newResourceLocker
	//then we need to start the manager
	newResourceLocker.Manager.Start()

	return reconcile.Result{}, nil
}

type ResourceLocker struct {
	LockedResources []unstructured.Unstructured
	Patches         []patchlocker.Patch
	Manager         stoppablemanager.StoppableManager
}

func getKeyFromInstance(instance *redhatcopv1alpha1.ResourceLocker) string {
	return types.NamespacedName{
		Name:      instance.GetName(),
		Namespace: instance.GetNamespace(),
	}.String()
}

func getLockedResources(instance *redhatcopv1alpha1.ResourceLocker) ([]unstructured.Unstructured, error) {
	objs := []unstructured.Unstructured{}
	for _, raw := range instance.Spec.Resources {
		bb, err := yaml.YAMLToJSON(raw.Raw)
		if err != nil {
			log.Error(err, "Error transforming yaml to json", "raw", raw.Raw)
			return []unstructured.Unstructured{}, err
		}
		obj := unstructured.Unstructured{}
		err = json.Unmarshal(bb, &obj)
		if err != nil {
			log.Error(err, "Error unmarshalling json manifest", "manifest", string(bb))
			return []unstructured.Unstructured{}, err
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

func (r *ReconcileResourceLocker) getResourceLockerFromInstance(instance *redhatcopv1alpha1.ResourceLocker) (*ResourceLocker, error) {
	lockedResources, err := getLockedResources(instance)
	if err != nil {
		return &ResourceLocker{}, err
	}
	patches, err := getPatches(instance)
	config, err := r.getRestConfigFromInstance(instance)
	if err != nil {
		return &ResourceLocker{}, err
	}
	stoppableManager, err := stoppablemanager.NewStoppableManager(config, manager.Options{})
	if err != nil {
		return &ResourceLocker{}, err
	}
	for _, lockedResource := range lockedResources {
		_, err := lockedresourcecontroller.NewLockedObjectReconciler(stoppableManager.Manager, lockedResource)
		if err != nil {
			return &ResourceLocker{}, err
		}
	}
	for _, patch := range patches {
		_, err := patchlocker.NewPatchLockerReconciler(stoppableManager.Manager, patch)
		if err != nil {
			return &ResourceLocker{}, err
		}
	}
	return &ResourceLocker{
		LockedResources: lockedResources,
		Patches:         patches,
		Manager:         stoppableManager,
	}, nil
}

func (r *ReconcileResourceLocker) getRestConfigFromInstance(instance *redhatcopv1alpha1.ResourceLocker) (*rest.Config, error) {
	sa := corev1.ServiceAccount{}
	err := r.GetClient().Get(context.TODO(), types.NamespacedName{Name: instance.Spec.ServiceAccountRef.Name, Namespace: instance.GetNamespace()}, &sa)
	if err != nil {
		log.Error(err, "unable to get the specified", "service account", types.NamespacedName{Name: instance.Spec.ServiceAccountRef.Name, Namespace: instance.GetNamespace()})
		return &rest.Config{}, err
	}
	var tokenSecret corev1.Secret
	for _, secretRef := range sa.Secrets {
		secret := corev1.Secret{}
		err := r.GetClient().Get(context.TODO(), types.NamespacedName{Name: secretRef.Name, Namespace: instance.GetNamespace()}, &secret)
		if err != nil {
			log.Error(err, "(ignoring) unable to get ", "ref secret", types.NamespacedName{Name: secretRef.Name, Namespace: instance.GetNamespace()})
			continue
		}
		if secret.Type == "kubernetes.io/service-account-token" {
			tokenSecret = secret
			break
		}
	}
	if tokenSecret.Data == nil {
		errs.New("unable to find secret of type kubernetes.io/service-account-token for service account")
	}
	//KUBERNETES_SERVICE_PORT=443
	//KUBERNETES_SERVICE_HOST=172.30.0.1
	kubehost, found := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	if !found {
		errs.New("unable to lookup environment variable KUBERNETES_SERVICE_HOST")
	}
	kubeport, found := os.LookupEnv("KUBERNETES_SERVICE_PORT")
	if !found {
		errs.New("unable to lookup environment variable KUBERNETES_SERVICE_PORT")
	}
	config := rest.Config{
		Host:        "https://" + kubehost + ":" + kubeport,
		BearerToken: string(tokenSecret.Data["token"]),
		TLSClientConfig: rest.TLSClientConfig{
			CAData: tokenSecret.Data["ca.crt"],
		},
	}
	return &config, nil
}

func getPatches(instance *redhatcopv1alpha1.ResourceLocker) ([]patchlocker.Patch, error) {
	patches := []patchlocker.Patch{}
	for _, patch := range instance.Spec.Patches {
		template, err := template.New(getPatchName(instance, &patch)).Parse(patch.PatchTemplate)
		if err != nil {
			log.Error(err, "unable to parse ", "template", patch.PatchTemplate)
			return []patchlocker.Patch{}, err
		}
		patches = append(patches, patchlocker.Patch{
			Patch:    patch,
			Template: *template,
		})
	}
	return patches, nil
}

func getPatchName(instance *redhatcopv1alpha1.ResourceLocker, patch *v1alpha1.Patch) string {
	return types.NamespacedName{
		Name:      instance.GetName(),
		Namespace: instance.GetNamespace(),
	}.String() + "-" + types.NamespacedName{
		Name:      patch.TargetObjectRef.Name,
		Namespace: patch.TargetObjectRef.Namespace,
	}.String()
}
