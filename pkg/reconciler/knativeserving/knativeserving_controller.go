/*
Copyright 2019 The Knative Authors

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

package knativeserving

import (
	"context"
	"fmt"

	mf "github.com/manifestival/manifestival"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/client-go/tools/cache"

	"knative.dev/pkg/controller"
	servingv1alpha1 "knative.dev/serving-operator/pkg/apis/serving/v1alpha1"
	listers "knative.dev/serving-operator/pkg/client/listers/serving/v1alpha1"
	"knative.dev/serving-operator/pkg/reconciler"
	"knative.dev/serving-operator/pkg/reconciler/knativeserving/common"
	"knative.dev/serving-operator/version"
)

const (
	finalizerName  = "delete-knative-serving-manifest"
	creationChange = "creation"
	editChange     = "edit"
	deletionChange = "deletion"
)

var (
	istioLabels   = map[string]string{"networking.knative.dev/ingress-provider": "istio"}
	autoTLSLabels = map[string]string{"networking.knative.dev/certificate-provider": "cert-manager", "networking.knative.dev/wildcard-certificate-provider": "nscert"}
)

// Reconciler implements controller.Reconciler for Knativeserving resources.
type Reconciler struct {
	*reconciler.Base
	// Listers index properties about resources
	knativeServingLister listers.KnativeServingLister
	config               mf.Manifest
	servings             map[string]int64
	// Platform-specific behavior to affect the transform
	platform common.Platforms
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Knativeserving resource
// with the current status of the resource.
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		r.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}
	// Get the KnativeServing resource with this namespace/name.
	original, err := r.knativeServingLister.KnativeServings(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		return nil
	} else if err != nil {
		r.Logger.Error(err, "Error getting KnativeServing")
		return err
	}
	if original.GetDeletionTimestamp() != nil {
		if _, ok := r.servings[key]; ok {
			delete(r.servings, key)
			r.StatsReporter.ReportKnativeservingChange(key, deletionChange)
		}
		return r.delete(original)
	}
	// Keep track of the number and generation of KnativeServings in the cluster.
	newGen := original.Generation
	if oldGen, ok := r.servings[key]; ok {
		if newGen > oldGen {
			r.StatsReporter.ReportKnativeservingChange(key, editChange)
		} else if newGen < oldGen {
			return fmt.Errorf("reconciling obsolete generation of KnativeServing %s: newGen = %d and oldGen = %d", key, newGen, oldGen)
		}
	} else {
		// No metrics are emitted when newGen > 1: the first reconciling of
		// a new operator on an existing KnativeServing resource.
		if newGen == 1 {
			r.StatsReporter.ReportKnativeservingChange(key, creationChange)
		}
	}
	r.servings[key] = newGen

	// Don't modify the informers copy.
	knativeServing := original.DeepCopy()

	// Reconcile this copy of the KnativeServing resource and then write back any status
	// updates regardless of whether the reconciliation errored out.
	reconcileErr := r.reconcile(ctx, knativeServing)
	if equality.Semantic.DeepEqual(original.Status, knativeServing.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.
	} else if err = r.updateStatus(knativeServing); err != nil {
		r.Logger.Warnw("Failed to update knativeServing status", zap.Error(err))
		r.Recorder.Eventf(knativeServing, corev1.EventTypeWarning, "UpdateFailed",
			"Failed to update status for KnativeServing %q: %v", knativeServing.Name, err)
		return err
	}
	if reconcileErr != nil {
		r.Recorder.Event(knativeServing, corev1.EventTypeWarning, "InternalError", reconcileErr.Error())
		return reconcileErr
	}
	return nil
}

func (r *Reconciler) reconcile(ctx context.Context, ks *servingv1alpha1.KnativeServing) error {
	reqLogger := r.Logger.With(zap.String("Request.Namespace", ks.Namespace)).With("Request.Name", ks.Name)
	reqLogger.Infow("Reconciling KnativeServing", "status", ks.Status)

	stages := []func(*mf.Manifest, *servingv1alpha1.KnativeServing) error{
		r.ensureFinalizer,
		r.initStatus,
		r.install,
		r.checkDeployments,
		r.deleteObsoleteResources,
	}

	manifest, err := r.transform(ks)
	if err != nil {
		ks.Status.MarkInstallFailed(err.Error())
		return err
	}

	manifest, err = r.filter(&manifest, ks)
	if err != nil {
		ks.Status.MarkInstallFailed(err.Error())
		return err
	}

	for _, stage := range stages {
		if err := stage(&manifest, ks); err != nil {
			return err
		}
	}
	reqLogger.Infow("Reconcile stages complete", "status", ks.Status)
	return nil
}

// Transform the resources
func (r *Reconciler) transform(instance *servingv1alpha1.KnativeServing) (mf.Manifest, error) {
	r.Logger.Debug("Transforming manifest")
	transforms, err := r.platform.Transformers(r.KubeClientSet, instance, r.Logger)
	if err != nil {
		return mf.Manifest{}, err
	}
	return r.config.Transform(transforms...)
}

// Filter the resources
func (r *Reconciler) filter(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) (mf.Manifest, error) {
	r.Logger.Debug("Filtering manifest")
	labels := map[string]string{}
	if !isIngressIstio(manifest, instance) {
		r.Logger.Debug("Removing istio extention resources")
		for k, v := range istioLabels {
			labels[k] = v
		}
	}
	if !isAutoTLSEnabled(manifest, instance) {
		r.Logger.Debug("Removing autoTLS extention resources")
		for k, v := range autoTLSLabels {
			labels[k] = v
		}
	}
	if len(labels) == 0 {
		return *manifest, nil
	}
	// TODO: manifestival's Delete() prints error logs but never returns error actually, so this IsNoMatchError does not make sense.
	// But leave this code here to show we cannot control "no matches for kind" erorr atm.
	if err := manifest.Filter(byLabels(labels)).Filter(mf.NotCRDs).Delete(); err != nil && !meta.IsNoMatchError(err) {
		return *manifest, err
	}
	return manifest.Filter(byNoLabels(labels)), nil
}

// Update the status subresource
func (r *Reconciler) updateStatus(instance *servingv1alpha1.KnativeServing) error {
	afterUpdate, err := r.KnativeServingClientSet.OperatorV1alpha1().KnativeServings(instance.Namespace).UpdateStatus(instance)

	if err != nil {
		return err
	}
	// TODO: We shouldn't rely on mutability and return the updated entities from functions instead.
	afterUpdate.DeepCopyInto(instance)
	return nil
}

// Initialize status conditions
func (r *Reconciler) initStatus(_ *mf.Manifest, instance *servingv1alpha1.KnativeServing) error {
	r.Logger.Debug("Initializing status")
	if len(instance.Status.Conditions) == 0 {
		instance.Status.InitializeConditions()
		if err := r.updateStatus(instance); err != nil {
			return err
		}
	}
	return nil
}

// Apply the manifest resources
func (r *Reconciler) install(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) error {
	r.Logger.Debug("Installing manifest")
	if err := manifest.Apply(); err != nil {
		instance.Status.MarkInstallFailed(err.Error())
		return err
	}
	instance.Status.MarkInstallSucceeded()
	instance.Status.Version = version.Version
	return nil
}

// byLabels returns true when the resource contains any of the specified key-label pairs.
func byLabels(labels map[string]string) mf.Predicate {
	return func(u *unstructured.Unstructured) bool {
		for key, value := range labels {
			if v := u.GetLabels()[key]; v == value {
				return true
			}
		}
		return false
	}
}

// byNoLabels returns true when the resource does not contain any of the specified key-label pairs.
func byNoLabels(labels map[string]string) mf.Predicate {
	return func(u *unstructured.Unstructured) bool {
		for key, value := range labels {
			if v := u.GetLabels()[key]; v == value {
				return false
			}
		}
		return true
	}
}

// isAutoTLSEnabled returns true when autoTLS is Enabled.
func isAutoTLSEnabled(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) bool {
	if autoTLS := instance.Spec.Config["network"]["autoTLS"]; autoTLS == "Enabled" {
		return true
	}
	return false
}

// isIngressIstio returns true when ingress.class is istio.ingress.networking.knative.dev or default(empty).
func isIngressIstio(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) bool {
	if _, ok := instance.Spec.Config["network"]; !ok {
		return true
	}
	if ingress := instance.Spec.Config["network"]["ingress.class"]; ingress == "istio.ingress.networking.knative.dev" || ingress == "" {
		return true
	}
	return false
}

// Check for all deployments available
func (r *Reconciler) checkDeployments(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) error {
	r.Logger.Debug("Checking deployments")
	available := func(d *appsv1.Deployment) bool {
		for _, c := range d.Status.Conditions {
			if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	}
	for _, u := range manifest.Filter(mf.ByKind("Deployment")).Resources() {
		deployment, err := r.KubeClientSet.AppsV1().Deployments(u.GetNamespace()).Get(u.GetName(), metav1.GetOptions{})
		if err != nil {
			instance.Status.MarkDeploymentsNotReady()
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !available(deployment) {
			instance.Status.MarkDeploymentsNotReady()
			return nil
		}
	}
	instance.Status.MarkDeploymentsAvailable()
	return nil
}

// ensureFinalizer attaches a "delete manifest" finalizer to the instance
func (r *Reconciler) ensureFinalizer(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) error {
	for _, finalizer := range instance.GetFinalizers() {
		if finalizer == finalizerName {
			return nil
		}
	}
	instance.SetFinalizers(append(instance.GetFinalizers(), finalizerName))
	instance, err := r.KnativeServingClientSet.OperatorV1alpha1().KnativeServings(instance.Namespace).Update(instance)
	return err
}

// delete all the resources in the release manifest
func (r *Reconciler) delete(instance *servingv1alpha1.KnativeServing) error {
	if len(instance.GetFinalizers()) == 0 || instance.GetFinalizers()[0] != finalizerName {
		return nil
	}
	if len(r.servings) == 0 {
		if err := r.config.Filter(mf.NotCRDs).Delete(); err != nil {
			return err
		}
	}
	// The deletionTimestamp might've changed. Fetch the resource again.
	refetched, err := r.knativeServingLister.KnativeServings(instance.Namespace).Get(instance.Name)
	if err != nil {
		return err
	}
	refetched.SetFinalizers(refetched.GetFinalizers()[1:])
	_, err = r.KnativeServingClientSet.OperatorV1alpha1().KnativeServings(refetched.Namespace).Update(refetched)
	return err
}

// Delete obsolete resources from previous versions
func (r *Reconciler) deleteObsoleteResources(manifest *mf.Manifest, instance *servingv1alpha1.KnativeServing) error {
	// istio-system resources from 0.3
	resource := &unstructured.Unstructured{}
	resource.SetNamespace("istio-system")
	resource.SetName("knative-ingressgateway")
	resource.SetAPIVersion("v1")
	resource.SetKind("Service")
	if err := manifest.Client.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("apps/v1")
	resource.SetKind("Deployment")
	if err := manifest.Client.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("autoscaling/v1")
	resource.SetKind("HorizontalPodAutoscaler")
	if err := manifest.Client.Delete(resource); err != nil {
		return err
	}
	// config-controller from 0.5
	resource.SetNamespace(instance.GetNamespace())
	resource.SetName("config-controller")
	resource.SetAPIVersion("v1")
	resource.SetKind("ConfigMap")
	if err := manifest.Client.Delete(resource); err != nil {
		return err
	}
	return nil
}
