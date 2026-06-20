package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type LoadOptions struct {
	Files       []string
	ProjectName string
	WorkingDir  string
	Environment []string
}

type Plan struct {
	Name       string
	Services   []ServicePlan
	Volumes    []VolumePlan
	Configs    []ConfigPlan
	Secrets    []string
	Normalized []byte
}

type ServicePlan struct {
	Name          string
	ContainerName string
	Spec          api.ContainerSpec
	Service       *corev1.Service
}

type VolumePlan struct {
	Name      string
	ClaimName string
	External  bool
}

type ConfigPlan struct {
	ContentHash string
	Name        string
	ConfigMap   *corev1.ConfigMap
}

func Load(ctx context.Context, options LoadOptions, namespace string) (*Plan, error) {
	parserOptions := []composecli.ProjectOptionsFn{
		composecli.WithOsEnv, composecli.WithDotEnv,
		composecli.WithEnvFiles(), composecli.WithDiscardEnvFile, composecli.WithResolvedPaths(true),
	}
	if options.WorkingDir != "" {
		parserOptions = append(parserOptions, composecli.WithWorkingDirectory(options.WorkingDir))
	}
	if options.ProjectName != "" {
		parserOptions = append(parserOptions, composecli.WithName(options.ProjectName))
	}
	if len(options.Environment) > 0 {
		parserOptions = append(parserOptions, composecli.WithEnv(options.Environment))
	}
	files := options.Files
	if len(files) == 0 {
		parserOptions = append(parserOptions, composecli.WithDefaultConfigPath)
	}
	projectOptions, err := composecli.NewProjectOptions(files, parserOptions...)
	if err != nil {
		return nil, fmt.Errorf("compose options: %w", err)
	}
	project, err := projectOptions.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	if err := validate(project); err != nil {
		return nil, err
	}
	normalized, err := project.MarshalYAML()
	if err != nil {
		return nil, err
	}
	return translate(project, namespace, normalized)
}

func validate(project *composetypes.Project) error {
	if len(project.Services) == 0 {
		return fmt.Errorf("compose project has no enabled services")
	}
	if len(project.Models) > 0 {
		return unsupported("top-level models")
	}
	for name, network := range project.Networks {
		if name != "default" || network.Driver != "" || bool(network.External) || len(network.DriverOpts) > 0 || network.EnableIPv4 != nil || network.EnableIPv6 != nil || network.Internal || network.Ipam.Config != nil || len(network.Labels) > 0 || network.Name != project.Name+"_default" {
			return unsupported("network " + name + " configuration")
		}
	}
	for name, volume := range project.Volumes {
		if volume.Driver != "" && volume.Driver != "local" {
			return unsupported("volume " + name + " driver")
		}
		if len(volume.DriverOpts) > 0 {
			return unsupported("volume " + name + " driver_opts")
		}
	}
	for name, config := range project.Configs {
		file := composetypes.FileObjectConfig(config)
		if file.External {
			return unsupported("external config " + name)
		}
		if file.Driver != "" || len(file.DriverOpts) > 0 || file.TemplateDriver != "" {
			return unsupported("config " + name + " driver options")
		}
		if file.File == "" && file.Content == "" && file.Environment == "" {
			return fmt.Errorf("config %s must define file, content, or environment", name)
		}
	}
	for name, secret := range project.Secrets {
		file := composetypes.FileObjectConfig(secret)
		if !file.External {
			return fmt.Errorf("secret %s must be external and reference an existing Kubernetes Secret", name)
		}
		if file.File != "" || file.Content != "" || file.Environment != "" || file.Driver != "" {
			return unsupported("inline secret " + name)
		}
	}
	for name, service := range project.Services {
		if dnsName(name) != name {
			return fmt.Errorf("service name %q is not a valid Kubernetes DNS name", name)
		}
		if err := validateServiceFields(name, service); err != nil {
			return err
		}
		if service.Image == "" {
			return fmt.Errorf("service %s: image is required; build is unsupported", name)
		}
		if service.Logging != nil || service.LogDriver != "" || len(service.LogOpt) > 0 {
			return unsupported("service " + name + " logging")
		}
		if service.Build != nil {
			return unsupported("service " + name + " build")
		}
		if service.Privileged {
			return unsupported("service " + name + " privileged")
		}
		if len(service.Devices) > 0 || len(service.DeviceCgroupRules) > 0 || len(service.Gpus) > 0 {
			return unsupported("service " + name + " devices")
		}
		if service.NetworkMode != "" || service.Net != "" {
			return unsupported("service " + name + " network_mode")
		}
		if service.Pid != "" || service.Ipc != "" || service.Uts != "" {
			return unsupported("service " + name + " host namespaces")
		}
		if service.User != "" {
			return unsupported("service " + name + " user (workloads run as the restricted dockube UID)")
		}
		if service.ContainerName != "" {
			return unsupported("service " + name + " container_name")
		}
		if service.Restart != "" || (service.Deploy != nil && service.Deploy.RestartPolicy != nil) {
			return unsupported("service " + name + " restart policy")
		}
		if service.GetScale() != 1 {
			return unsupported("service " + name + " replicas other than 1")
		}
		if len(service.CapAdd) > 0 || len(service.CapDrop) > 0 || len(service.SecurityOpt) > 0 || len(service.Sysctls) > 0 {
			return unsupported("service " + name + " security overrides")
		}
		if len(service.ExtraHosts) > 0 || len(service.DNS) > 0 || len(service.DNSOpts) > 0 || len(service.DNSSearch) > 0 || service.Hostname != "" || service.DomainName != "" {
			return unsupported("service " + name + " DNS/host overrides")
		}
		if len(service.Tmpfs) > 0 || len(service.VolumesFrom) > 0 {
			return unsupported("service " + name + " non-named volumes")
		}
		for _, volume := range service.Volumes {
			if volume.Type != composetypes.VolumeTypeVolume || volume.Source == "" {
				return unsupported("service " + name + " host, anonymous, tmpfs, image, or CSI mounts")
			}
			if volume.Volume != nil && (volume.Volume.NoCopy || volume.Volume.Subpath != "") {
				return unsupported("service " + name + " volume options")
			}
		}
		for dependency, config := range service.DependsOn {
			if _, ok := project.Services[dependency]; !ok {
				return fmt.Errorf("service %s depends on unknown service %s", name, dependency)
			}
			if config.Condition != "" && config.Condition != composetypes.ServiceConditionStarted {
				return unsupported("service " + name + " depends_on condition " + config.Condition)
			}
		}
		if service.Deploy != nil {
			deploy := service.Deploy
			if deploy.Mode != "" || len(deploy.Labels) > 0 || deploy.UpdateConfig != nil || deploy.RollbackConfig != nil || !reflect.DeepEqual(deploy.Placement, composetypes.Placement{}) || deploy.EndpointMode != "" {
				return unsupported("service " + name + " deploy settings other than resources")
			}
			for _, resources := range []*composetypes.Resource{deploy.Resources.Limits, deploy.Resources.Reservations} {
				if resources != nil && (resources.Pids != 0 || len(resources.Devices) > 0 || len(resources.GenericResources) > 0) {
					return unsupported("service " + name + " non-CPU/memory resources")
				}
			}
		}
		if service.HealthCheck != nil && !service.HealthCheck.Disable {
			test := service.HealthCheck.Test
			if len(test) < 2 || (test[0] != "CMD" && test[0] != "CMD-SHELL") {
				return unsupported("service " + name + " healthcheck form")
			}
		}
		for _, port := range service.Ports {
			if port.Target == 0 || port.Protocol == "sctp" || port.HostIP != "" || (port.Mode != "" && port.Mode != "ingress") {
				return unsupported("service " + name + " port options")
			}
		}
	}
	return nil
}

func validateServiceFields(name string, service composetypes.ServiceConfig) error {
	allowed := map[string]bool{"Name": true, "Command": true, "Configs": true, "CustomLabels": true, "DependsOn": true, "Deploy": true, "Entrypoint": true, "Environment": true, "EnvFiles": true, "Expose": true, "HealthCheck": true, "Image": true, "Networks": true, "Ports": true, "Secrets": true, "StdinOpen": true, "Tty": true, "Volumes": true, "WorkingDir": true}
	value := reflect.ValueOf(service)
	typ := value.Type()
	for i := 0; i < value.NumField(); i++ {
		if allowed[typ.Field(i).Name] || value.Field(i).IsZero() {
			continue
		}
		field := typ.Field(i).Tag.Get("yaml")
		field = strings.Split(field, ",")[0]
		if field == "" || field == "-" {
			field = typ.Field(i).Name
		}
		return unsupported("service " + name + " " + field)
	}
	return nil
}

func unsupported(field string) error { return fmt.Errorf("unsupported Compose field: %s", field) }

func translate(project *composetypes.Project, namespace string, normalized []byte) (*Plan, error) {
	project.Name = dnsName(project.Name)
	plan := &Plan{Name: project.Name, Normalized: normalized}
	volumeNames := sortedKeys(project.Volumes)
	for _, name := range volumeNames {
		volume := project.Volumes[name]
		claimName := dnsName(project.Name + "-" + name)
		if volume.Name != "" {
			claimName = dnsName(volume.Name)
		}
		plan.Volumes = append(plan.Volumes, VolumePlan{Name: name, ClaimName: claimName, External: bool(volume.External)})
	}
	configNames := sortedKeys(project.Configs)
	for _, name := range configNames {
		config := composetypes.FileObjectConfig(project.Configs[name])
		content, err := configContent(config)
		if err != nil {
			return nil, fmt.Errorf("config %s: %w", name, err)
		}
		cmName := dnsName(project.Name + "-config-" + name)
		plan.Configs = append(plan.Configs, ConfigPlan{Name: name, ContentHash: shortHash(content), ConfigMap: &corev1.ConfigMap{ObjectMeta: objectMeta(cmName, namespace, project.Name), Data: map[string]string{"content": content}}})
	}
	secretNames := sortedKeys(project.Secrets)
	for _, name := range secretNames {
		secret := composetypes.FileObjectConfig(project.Secrets[name])
		secretName := name
		if secret.Name != "" {
			secretName = secret.Name
		}
		plan.Secrets = append(plan.Secrets, secretName)
	}
	serviceNames := project.ServiceNames()
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		service := project.Services[name]
		servicePlan, err := translateService(project, name, service, namespace, plan)
		if err != nil {
			return nil, err
		}
		plan.Services = append(plan.Services, servicePlan)
	}
	return plan, nil
}

func translateService(project *composetypes.Project, name string, service composetypes.ServiceConfig, namespace string, plan *Plan) (ServicePlan, error) {
	containerName := dnsName(project.Name + "-" + name)
	spec := api.ContainerSpec{Image: service.Image, Command: []string(service.Entrypoint), Args: []string(service.Command), DesiredState: "Running", Stdin: service.StdinOpen, TTY: service.Tty, WorkingDir: service.WorkingDir, Labels: map[string]string{api.ProjectLabel: project.Name, api.ServiceLabel: name}}
	keys := make([]string, 0, len(service.Environment))
	for key := range service.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if service.Environment[key] != nil {
			spec.Env = append(spec.Env, corev1.EnvVar{Name: key, Value: *service.Environment[key]})
		}
	}
	ports, servicePorts := translatePorts(service)
	spec.Ports = ports
	for index, volume := range service.Volumes {
		claim := findVolume(plan, volume.Source)
		if claim == "" {
			return ServicePlan{}, fmt.Errorf("service %s references unknown volume %s", name, volume.Source)
		}
		spec.Mounts = append(spec.Mounts, api.Mount{Name: fmt.Sprintf("volume-%d", index), Type: "pvc", Source: claim, Target: volume.Target, ReadOnly: volume.ReadOnly})
	}
	for index, ref := range service.Configs {
		cm := findConfig(plan, ref.Source)
		if cm == "" {
			return ServicePlan{}, fmt.Errorf("service %s references unknown config %s", name, ref.Source)
		}
		target := ref.Target
		if target == "" {
			target = "/" + ref.Source
		}
		spec.Mounts = append(spec.Mounts, api.Mount{Name: fmt.Sprintf("config-%d", index), Type: "configMap", Source: cm, Target: target, ReadOnly: true, SubPath: "content", Items: map[string]string{"content": "content"}, ContentHash: findConfigHash(plan, ref.Source)})
	}
	for index, ref := range service.Secrets {
		secret := findSecret(project, ref.Source)
		if secret == "" {
			return ServicePlan{}, fmt.Errorf("service %s references unknown secret %s", name, ref.Source)
		}
		target := ref.Target
		if target == "" {
			target = "/run/secrets/" + ref.Source
		}
		spec.Mounts = append(spec.Mounts, api.Mount{Name: fmt.Sprintf("secret-%d", index), Type: "secret", Source: secret, Target: target, ReadOnly: true, SubPath: ref.Source, Items: map[string]string{ref.Source: ref.Source}})
	}
	spec.Resources = translateResources(service)
	if service.HealthCheck != nil && !service.HealthCheck.Disable {
		probe := translateProbe(service.HealthCheck)
		spec.Readiness = probe.DeepCopy()
		spec.Liveness = probe.DeepCopy()
		if service.HealthCheck.StartPeriod != nil {
			spec.Startup = probe.DeepCopy()
		}
	}
	selector := map[string]string{api.NameLabel: containerName}
	clusterIP := ""
	if len(servicePorts) == 0 {
		clusterIP = corev1.ClusterIPNone
	}
	kubeService := &corev1.Service{ObjectMeta: objectMeta(dnsName(name), namespace, project.Name), Spec: corev1.ServiceSpec{Selector: selector, Ports: servicePorts, ClusterIP: clusterIP}}
	kubeService.Labels[api.ServiceLabel] = name
	return ServicePlan{Name: name, ContainerName: containerName, Spec: spec, Service: kubeService}, nil
}

func translatePorts(service composetypes.ServiceConfig) ([]corev1.ContainerPort, []corev1.ServicePort) {
	seen := map[string]bool{}
	var containers []corev1.ContainerPort
	var services []corev1.ServicePort
	add := func(target int32, protocol corev1.Protocol, published string) {
		key := fmt.Sprintf("%d/%s", target, protocol)
		if seen[key] {
			return
		}
		seen[key] = true
		name := fmt.Sprintf("p%d-%s", target, strings.ToLower(string(protocol)))
		containers = append(containers, corev1.ContainerPort{Name: name, ContainerPort: target, Protocol: protocol})
		port := target
		if value, err := strconv.Atoi(published); err == nil && value > 0 && value <= 65535 {
			port = int32(value)
		}
		services = append(services, corev1.ServicePort{Name: name, Port: port, TargetPort: intstr.FromInt32(target), Protocol: protocol})
	}
	for _, port := range service.Ports {
		add(int32(port.Target), protocol(port.Protocol), port.Published)
	}
	for _, exposed := range service.Expose {
		parts := strings.SplitN(exposed, "/", 2)
		value, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		proto := "tcp"
		if len(parts) == 2 {
			proto = parts[1]
		}
		add(int32(value), protocol(proto), "")
	}
	return containers, services
}

func translateResources(service composetypes.ServiceConfig) corev1.ResourceRequirements {
	result := corev1.ResourceRequirements{}
	if service.Deploy == nil {
		return result
	}
	convert := func(value *composetypes.Resource) corev1.ResourceList {
		if value == nil {
			return nil
		}
		list := corev1.ResourceList{}
		if value.NanoCPUs > 0 {
			list[corev1.ResourceCPU] = resource.MustParse(strconv.FormatFloat(float64(value.NanoCPUs), 'f', -1, 32))
		}
		if value.MemoryBytes > 0 {
			list[corev1.ResourceMemory] = *resource.NewQuantity(int64(value.MemoryBytes), resource.BinarySI)
		}
		return list
	}
	result.Limits = convert(service.Deploy.Resources.Limits)
	result.Requests = convert(service.Deploy.Resources.Reservations)
	return result
}

func translateProbe(health *composetypes.HealthCheckConfig) *corev1.Probe {
	command := []string(health.Test[1:])
	if health.Test[0] == "CMD-SHELL" {
		command = []string{"/bin/sh", "-c", strings.Join(command, " ")}
	}
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}}, TimeoutSeconds: 1, PeriodSeconds: 30, FailureThreshold: 3}
	if health.Timeout != nil {
		probe.TimeoutSeconds = seconds(time.Duration(*health.Timeout))
	}
	if health.Interval != nil {
		probe.PeriodSeconds = seconds(time.Duration(*health.Interval))
	}
	if health.Retries != nil {
		probe.FailureThreshold = int32(*health.Retries)
	}
	if health.StartPeriod != nil {
		probe.InitialDelaySeconds = seconds(time.Duration(*health.StartPeriod))
	}
	return probe
}

func seconds(duration time.Duration) int32 {
	value := int32(duration.Round(time.Second) / time.Second)
	if value < 1 {
		return 1
	}
	return value
}
func protocol(value string) corev1.Protocol {
	if strings.EqualFold(value, "udp") {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}
func objectMeta(name, namespace, project string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{api.ManagedByLabel: api.ManagedByValue, api.ProjectLabel: project}}
}
func configContent(config composetypes.FileObjectConfig) (string, error) {
	if config.Content != "" {
		return config.Content, nil
	}
	if config.Environment != "" {
		value, ok := os.LookupEnv(config.Environment)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", config.Environment)
		}
		return value, nil
	}
	raw, err := os.ReadFile(filepath.Clean(config.File))
	return string(raw), err
}
func findVolume(plan *Plan, name string) string {
	for _, volume := range plan.Volumes {
		if volume.Name == name {
			return volume.ClaimName
		}
	}
	return ""
}
func findConfigHash(plan *Plan, name string) string {
	for _, config := range plan.Configs {
		if config.Name == name {
			return config.ContentHash
		}
	}
	return ""
}

func findConfig(plan *Plan, name string) string {
	for _, config := range plan.Configs {
		if config.Name == name {
			return config.ConfigMap.Name
		}
	}
	return ""
}
func findSecret(project *composetypes.Project, name string) string {
	secret, ok := project.Secrets[name]
	if !ok {
		return ""
	}
	value := composetypes.FileObjectConfig(secret)
	if value.Name != "" {
		return value.Name
	}
	return name
}
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func dnsName(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	value = strings.Trim(b.String(), "-")
	if value == "" {
		value = "dockube"
	}
	if len(value) <= 63 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return strings.Trim(value[:54], "-") + "-" + hex.EncodeToString(sum[:])[:8]
}
func sortedKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
