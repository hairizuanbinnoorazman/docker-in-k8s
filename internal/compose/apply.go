package compose

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type Clients struct {
	Dynamic dynamic.Interface
	Core    kubernetes.Interface
}

func (p *Plan) Apply(ctx context.Context, clients Clients, namespace string) error {
	if err := p.preflight(ctx, clients, namespace); err != nil {
		return err
	}
	for _, volume := range p.Volumes {
		if volume.External {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: objectMeta(volume.ClaimName, namespace, p.Name), Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
		if _, err := clients.Core.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create volume %s: %w", volume.Name, err)
		}
	}
	for _, config := range p.Configs {
		if err := applyConfigMap(ctx, clients.Core, config.ConfigMap); err != nil {
			return fmt.Errorf("apply config %s: %w", config.Name, err)
		}
	}
	for _, service := range p.Services {
		if err := applyContainer(ctx, clients.Dynamic, namespace, service.ContainerName, service.Spec); err != nil {
			return fmt.Errorf("apply service %s workload: %w", service.Name, err)
		}
		if err := applyService(ctx, clients.Core, service.Service); err != nil {
			return fmt.Errorf("apply service %s DNS: %w", service.Name, err)
		}
	}
	return p.removeStale(ctx, clients, namespace)
}

func (p *Plan) preflight(ctx context.Context, clients Clients, namespace string) error {
	var result error
	for _, secret := range p.Secrets {
		if _, err := clients.Core.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{}); err != nil {
			result = errors.Join(result, fmt.Errorf("required Kubernetes Secret %s: %w", secret, err))
		}
	}
	for _, volume := range p.Volumes {
		if volume.External {
			if _, err := clients.Core.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, volume.ClaimName, metav1.GetOptions{}); err != nil {
				result = errors.Join(result, fmt.Errorf("external volume %s requires PVC %s: %w", volume.Name, volume.ClaimName, err))
			}
		}
	}
	for _, service := range p.Services {
		existing, err := clients.Core.CoreV1().Services(namespace).Get(ctx, service.Service.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if existing.Labels[api.ProjectLabel] != p.Name || existing.Labels[api.ManagedByLabel] != api.ManagedByValue {
			result = errors.Join(result, fmt.Errorf("Service %s already exists and is not owned by Compose project %s", service.Service.Name, p.Name))
		}
	}
	return result
}

func applyContainer(ctx context.Context, client dynamic.Interface, namespace, name string, spec api.ContainerSpec) error {
	resourceClient := client.Resource(api.GVR).Namespace(namespace)
	existing, err := resourceClient.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = resourceClient.Create(ctx, api.NewContainer(name, namespace, spec), metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.GetLabels()[api.ProjectLabel] != spec.Labels[api.ProjectLabel] {
		return fmt.Errorf("DockerContainer %s is owned by another project", name)
	}
	labels := existing.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[api.ManagedByLabel] = api.ManagedByValue
	for key, value := range spec.Labels {
		labels[key] = value
	}
	existing.SetLabels(labels)
	if err := api.SetSpec(existing, spec); err != nil {
		return err
	}
	_, err = resourceClient.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func applyConfigMap(ctx context.Context, client kubernetes.Interface, desired *corev1.ConfigMap) error {
	maps := client.CoreV1().ConfigMaps(desired.Namespace)
	existing, err := maps.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = maps.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[api.ProjectLabel] != desired.Labels[api.ProjectLabel] {
		return fmt.Errorf("ConfigMap %s is owned by another project", desired.Name)
	}
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	_, err = maps.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func applyService(ctx context.Context, client kubernetes.Interface, desired *corev1.Service) error {
	services := client.CoreV1().Services(desired.Namespace)
	existing, err := services.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = services.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.Spec.ClusterIPs = existing.Spec.ClusterIPs
	desired.Spec.IPFamilies = existing.Spec.IPFamilies
	desired.Spec.IPFamilyPolicy = existing.Spec.IPFamilyPolicy
	desired.Spec.InternalTrafficPolicy = existing.Spec.InternalTrafficPolicy
	_, err = services.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func (p *Plan) removeStale(ctx context.Context, clients Clients, namespace string) error {
	wantedContainers := map[string]bool{}
	wantedServices := map[string]bool{}
	wantedConfigs := map[string]bool{}
	for _, service := range p.Services {
		wantedContainers[service.ContainerName] = true
		wantedServices[service.Service.Name] = true
	}
	for _, config := range p.Configs {
		wantedConfigs[config.ConfigMap.Name] = true
	}
	selector := api.ProjectLabel + "=" + p.Name
	containers, err := clients.Dynamic.Resource(api.GVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range containers.Items {
		if !wantedContainers[containers.Items[i].GetName()] {
			if err := clients.Dynamic.Resource(api.GVR).Namespace(namespace).Delete(ctx, containers.Items[i].GetName(), metav1.DeleteOptions{}); err != nil {
				return err
			}
		}
	}
	services, err := clients.Core.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range services.Items {
		if !wantedServices[services.Items[i].Name] {
			if err := clients.Core.CoreV1().Services(namespace).Delete(ctx, services.Items[i].Name, metav1.DeleteOptions{}); err != nil {
				return err
			}
		}
	}
	configs, err := clients.Core.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range configs.Items {
		if !wantedConfigs[configs.Items[i].Name] {
			if err := clients.Core.CoreV1().ConfigMaps(namespace).Delete(ctx, configs.Items[i].Name, metav1.DeleteOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func List(ctx context.Context, clients Clients, namespace, project string) ([]unstructured.Unstructured, error) {
	list, err := clients.Dynamic.Resource(api.GVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: api.ProjectLabel + "=" + project})
	if err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].GetLabels()[api.ServiceLabel] < list.Items[j].GetLabels()[api.ServiceLabel]
	})
	return list.Items, nil
}

func SetState(ctx context.Context, clients Clients, namespace, project, state string) error {
	items, err := List(ctx, clients, namespace, project)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("Compose project %s has no workloads", project)
	}
	for i := range items {
		spec, err := api.Spec(&items[i])
		if err != nil {
			return err
		}
		spec.DesiredState = state
		if err := api.SetSpec(&items[i], spec); err != nil {
			return err
		}
		if _, err := clients.Dynamic.Resource(api.GVR).Namespace(namespace).Update(ctx, &items[i], metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func Down(ctx context.Context, clients Clients, namespace, project string, volumes bool) error {
	selector := api.ProjectLabel + "=" + project
	items, err := clients.Dynamic.Resource(api.GVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range items.Items {
		if err := clients.Dynamic.Resource(api.GVR).Namespace(namespace).Delete(ctx, items.Items[i].GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	services, err := clients.Core.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range services.Items {
		if err := clients.Core.CoreV1().Services(namespace).Delete(ctx, services.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	configs, err := clients.Core.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range configs.Items {
		if err := clients.Core.CoreV1().ConfigMaps(namespace).Delete(ctx, configs.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if volumes {
		claims, err := clients.Core.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		for i := range claims.Items {
			if err := clients.Core.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, claims.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(items.Items) > 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		items, err = clients.Dynamic.Resource(api.GVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
	}
	if len(items.Items) > 0 {
		return fmt.Errorf("timed out removing Compose project %s workloads", project)
	}
	return nil
}
