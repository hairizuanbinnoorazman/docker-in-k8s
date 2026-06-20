package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type Reconciler struct {
	dynamic   dynamic.Interface
	core      kubernetes.Interface
	namespace string
}

func New(dynamicClient dynamic.Interface, coreClient kubernetes.Interface, namespace string) *Reconciler {
	return &Reconciler{dynamic: dynamicClient, core: coreClient, namespace: namespace}
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.ReconcileAll(ctx); err != nil {
			fmt.Printf("reconcile error: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	list, err := r.dynamic.Resource(api.GVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: api.ManagedByLabel + "=" + api.ManagedByValue,
	})
	if err != nil {
		return err
	}
	var result error
	for i := range list.Items {
		if err := r.Reconcile(ctx, &list.Items[i]); err != nil {
			result = errors.Join(result, fmt.Errorf("%s/%s: %w", list.Items[i].GetNamespace(), list.Items[i].GetName(), err))
		}
	}
	return result
}

func (r *Reconciler) Reconcile(ctx context.Context, obj *unstructured.Unstructured) error {
	if !obj.GetDeletionTimestamp().IsZero() {
		return r.finalize(ctx, obj)
	}
	if !containsString(obj.GetFinalizers(), api.Finalizer) {
		copy := obj.DeepCopy()
		copy.SetFinalizers(append(copy.GetFinalizers(), api.Finalizer))
		_, err := r.dynamic.Resource(api.GVR).Namespace(obj.GetNamespace()).Update(ctx, copy, metav1.UpdateOptions{})
		return err
	}

	spec, err := api.Spec(obj)
	if err != nil {
		return err
	}
	pods := r.core.CoreV1().Pods(obj.GetNamespace())
	podName := api.PodName(obj.GetName(), string(obj.GetUID()))

	if spec.DesiredState != "Running" {
		err := pods.Delete(ctx, podName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return r.updateStoppedStatus(ctx, obj)
	}

	pod, err := pods.Get(ctx, podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		pod, err = pods.Create(ctx, api.PodFor(obj, spec), metav1.CreateOptions{})
	}
	if err != nil {
		return err
	}
	return r.updateStatus(ctx, obj, pod)
}

func (r *Reconciler) finalize(ctx context.Context, obj *unstructured.Unstructured) error {
	if !containsString(obj.GetFinalizers(), api.Finalizer) {
		return nil
	}
	podName := api.PodName(obj.GetName(), string(obj.GetUID()))
	pods := r.core.CoreV1().Pods(obj.GetNamespace())
	_, err := pods.Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		zero := int64(0)
		return pods.Delete(ctx, podName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	copy := obj.DeepCopy()
	finalizers := make([]string, 0, len(copy.GetFinalizers()))
	for _, finalizer := range copy.GetFinalizers() {
		if finalizer != api.Finalizer {
			finalizers = append(finalizers, finalizer)
		}
	}
	copy.SetFinalizers(finalizers)
	_, err = r.dynamic.Resource(api.GVR).Namespace(obj.GetNamespace()).Update(ctx, copy, metav1.UpdateOptions{})
	return err
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *Reconciler) updateStatus(ctx context.Context, obj *unstructured.Unstructured, pod *corev1.Pod) error {
	copy := obj.DeepCopy()
	copy.Object["status"] = api.StatusMap(obj, pod)
	_, err := r.dynamic.Resource(api.GVR).Namespace(obj.GetNamespace()).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func (r *Reconciler) updateStoppedStatus(ctx context.Context, obj *unstructured.Unstructured) error {
	copy := obj.DeepCopy()
	copy.Object["status"] = map[string]any{
		"containerID": api.ContainerID(string(obj.GetUID())),
		"phase":       "Stopped",
	}
	_, err := r.dynamic.Resource(api.GVR).Namespace(obj.GetNamespace()).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}
