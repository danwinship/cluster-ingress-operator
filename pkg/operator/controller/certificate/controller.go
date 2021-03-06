// The certificate controller is responsible for:
//
//   1. Managing a CA for minting self-signed certs
//   2. Managing self-signed certificates for any clusteringresses which require them
//   3. Publishing the CA to `openshift-config-managed`
//   4. Publishing in-use certificates to `openshift-config-managed`
package certificate

import (
	"context"
	"fmt"
	"time"

	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/client-go/tools/record"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	controllerName = "certificate-controller"
)

var log = logf.Logger.WithName(controllerName)

func New(mgr manager.Manager, client client.Client, operatorNamespace string) (runtimecontroller.Controller, error) {
	reconciler := &reconciler{
		client:            client,
		recorder:          mgr.GetRecorder(controllerName),
		operatorNamespace: operatorNamespace,
	}
	c, err := runtimecontroller.New(controllerName, mgr, runtimecontroller.Options{Reconciler: reconciler})
	if err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &operatorv1.IngressController{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return nil, err
	}
	return c, nil
}

type reconciler struct {
	client            client.Client
	recorder          record.EventRecorder
	operatorNamespace string
}

func (r *reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	ca, err := r.ensureRouterCASecret()
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to ensure router CA: %v", err)
	}

	defaultCertificateChanged := false
	result := reconcile.Result{}
	errs := []error{}
	ingress := &operatorv1.IngressController{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, ingress); err != nil {
		if errors.IsNotFound(err) {
			// The ingress could have been deleted and we're processing a stale queue
			// item, so ignore and skip.
			log.Info("clusteringress not found; reconciliation will be skipped", "request", request)
		} else {
			errs = append(errs, fmt.Errorf("failed to get clusteringress: %v", err))
		}
	} else {
		deployment := &appsv1.Deployment{}
		err = r.client.Get(context.TODO(), controller.RouterDeploymentName(ingress), deployment)
		if err != nil {
			if errors.IsNotFound(err) {
				// All ingresses should have a deployment, so this one may not have been
				// created yet. Retry after a reasonable amount of time.
				log.Info("deployment not found; will retry default cert sync", "clusteringress", ingress.Name)
				result.RequeueAfter = 5 * time.Second
			} else {
				errs = append(errs, fmt.Errorf("failed to get deployment: %v", err))
			}
		} else {
			trueVar := true
			deploymentRef := metav1.OwnerReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployment.Name,
				UID:        deployment.UID,
				Controller: &trueVar,
			}
			defaultCertificateChanged, err = r.ensureDefaultCertificateForIngress(ca, deployment.Namespace, deploymentRef, ingress)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to ensure default cert for %s: %v", ingress.Name, err))
			}
		}
	}

	ingresses := &operatorv1.IngressControllerList{}
	if err := r.client.List(context.TODO(), &client.ListOptions{Namespace: r.operatorNamespace}, ingresses); err != nil {
		errs = append(errs, fmt.Errorf("failed to list clusteringresses: %v", err))
	} else {
		if err := r.ensureRouterCAConfigMap(ca, ingresses.Items); err != nil {
			errs = append(errs, fmt.Errorf("failed to publish router CA: %v", err))
		}

		if defaultCertificateChanged {
			secrets := &corev1.SecretList{}
			if err := r.client.List(context.TODO(), &client.ListOptions{Namespace: "openshift-ingress"}, secrets); err != nil {
				errs = append(errs, fmt.Errorf("failed to list secrets: %v", err))
			} else {
				if err := r.ensureRouterCertsGlobalSecret(secrets.Items, ingresses.Items); err != nil {
					errs = append(errs, fmt.Errorf("failed to ensure router-certs secret: %v", err))
				}
			}
		}
	}

	return result, utilerrors.NewAggregate(errs)
}
